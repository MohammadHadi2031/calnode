package handler

// Hosts adding guests to an existing booking (wisegrid fork, checklist R4). See
// docs/fork-host-guests.md.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/calnode/calnode/internal/booking"
	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/mailer"
	"github.com/calnode/calnode/internal/uid"
)

// maxBookingGuests caps the guests on one booking whoever adds them: the bound the schema
// puts on an event type's max_guests. A host is not held to the booker's lower cap.
const maxBookingGuests = 50

// addGuestBudget bounds the invite a guest add sends, which runs inside the request so the
// host learns whether it went out.
var addGuestBudget = 45 * time.Second

type guestJSON struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// ListBookingGuests handles GET /v1/bookings/{id}/guests: the booking's guests, in the
// order they were added (the booker is not one of them).
func (h *Handler) ListBookingGuests(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	id := r.PathValue("id")
	if !h.canManageBooking(w, r, user, id) {
		return
	}
	guests, err := h.bookingSvc.Guests(r.Context(), id)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "list guests", "error", err, "booking_id", id)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"items": toGuestJSON(guests)})
}

func toGuestJSON(guests []booking.Attendee) []guestJSON {
	out := make([]guestJSON, 0, len(guests))
	for _, g := range guests {
		out = append(out, guestJSON{Name: g.Name, Email: g.Email})
	}
	return out
}

// AddBookingGuest handles POST /v1/bookings/{id}/guests {"email", "name"?}: a host (or an
// admin) adds a guest to an upcoming confirmed booking and the guest is invited at once -
// by the meeting's calendar event when its provider can add attendees and invites them
// itself, else by Calnode's own confirmation email with the .ics. Later reschedules and
// cancellations reach the guest like any other (they read the booking's guest list).
func (h *Handler) AddBookingGuest(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	email, err := normalizeBookerEmail(strings.TrimSpace(req.Email))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "a valid email address is required")
		return
	}
	name := strings.TrimSpace(req.Name)
	if len([]rune(name)) > 200 {
		h.writeError(w, http.StatusBadRequest, "name is too long")
		return
	}
	if !h.canManageBooking(w, r, user, id) {
		return
	}
	ctx := r.Context()
	b, err := h.bookingSvc.Get(ctx, id)
	if err != nil {
		h.logger.ErrorContext(ctx, "add guest: load booking", "error", err, "booking_id", id)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if b.Status != "confirmed" {
		h.writeError(w, http.StatusConflict, "guests can only be added to a confirmed booking")
		return
	}
	if !b.EndAt.After(time.Now()) {
		h.writeError(w, http.StatusConflict, "this booking has already ended")
		return
	}

	// The booker and the hosts already attend; listing them as guests would send them a
	// second invite.
	var bookerEmail, bookerTZ, bookerLocale string
	if err := h.db.QueryRowContext(ctx, `
		SELECT email, iana_timezone, COALESCE(locale, 'en') FROM booking_attendees
		WHERE booking_id = ? AND is_organizer = 1`, b.ID).Scan(&bookerEmail, &bookerTZ, &bookerLocale); err != nil {
		h.logger.ErrorContext(ctx, "add guest: load booker", "error", err, "booking_id", b.ID)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	hosts, err := h.assignedHosts(ctx, b.ID)
	if err != nil {
		h.logger.ErrorContext(ctx, "add guest: load hosts", "error", err, "booking_id", b.ID)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if strings.EqualFold(email, bookerEmail) {
		h.writeError(w, http.StatusConflict, "that address is the booker's")
		return
	}
	for _, host := range hosts {
		if strings.EqualFold(email, host.Email) {
			h.writeError(w, http.StatusConflict, "that address belongs to a host of this booking")
			return
		}
	}
	guests, err := h.bookingSvc.Guests(ctx, b.ID)
	if err != nil {
		h.logger.ErrorContext(ctx, "add guest: load guests", "error", err, "booking_id", b.ID)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	for _, g := range guests {
		if strings.EqualFold(email, g.Email) {
			h.writeError(w, http.StatusConflict, "that guest is already invited")
			return
		}
	}
	if len(guests) >= maxBookingGuests {
		h.writeError(w, http.StatusConflict, "this booking already has the maximum number of guests")
		return
	}
	// Guests take the booker's zone and language: that is what the booking's emails and
	// invites to them are written in (see the booking-time insert in booking.Service).
	if _, err := h.db.ExecContext(ctx, `
		INSERT INTO booking_attendees (id, booking_id, name, email, iana_timezone, is_organizer, locale)
		VALUES (?, ?, ?, ?, ?, 0, ?)`, uid.New(), b.ID, name, email, bookerTZ, bookerLocale); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			h.writeError(w, http.StatusConflict, "that guest is already invited")
			return
		}
		h.logger.ErrorContext(ctx, "add guest: insert", "error", err, "booking_id", b.ID)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	inviteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), addGuestBudget)
	defer cancel()
	via, inviteErr := h.inviteNewGuest(inviteCtx, b, hosts, mailer.Invitee{Name: name, Email: email})
	resp := map[string]any{"guest": guestJSON{Name: name, Email: email}, "invited_via": via}
	if inviteErr != nil {
		// The guest stays on the booking (reschedule/cancel will reach them); the host is
		// told the invite itself did not go out, and the email log has the detail.
		resp["warning"] = "The guest was added but the invitation could not be sent: " + inviteErr.Error()
	}
	if updated, err := h.bookingSvc.Guests(ctx, b.ID); err == nil {
		resp["guests"] = toGuestJSON(updated)
	}
	h.writeJSON(w, http.StatusCreated, resp)
}

// inviteNewGuest invites a guest just added to b. A calendar-invited booking adds them to
// each event that holds the meeting (the shared organizer event, or every host's own), and
// the provider sends the invite - the same way the booking's other guests were invited, so
// Google Meet admits them without knocking. When there is no such event, or any of them
// cannot take the attendee, Calnode emails the guest their own confirmation with the .ics
// instead: an invite from both is better than none. Returns "calendar" or "email".
func (h *Handler) inviteNewGuest(ctx context.Context, b *booking.Booking, hosts []assignedHost, g mailer.Invitee) (string, error) {
	if gc := h.getCal(); gc != nil && b.InviteDelivery != booking.InviteByCalnode {
		attempted, failed := false, false
		for _, host := range hosts {
			if host.InvitedViaOrganizer || host.ExternalEventID == "" {
				continue // no event of their own: they are on the organizer's
			}
			attempted = true
			err := gc.AddAttendees(ctx, host.UserID, host.ExternalCalendarID, host.ExternalEventID,
				host.ExternalProvider, []calendar.Invitee{{Name: g.Name, Email: g.Email}})
			if err != nil {
				failed = true
				if !errors.Is(err, calendar.ErrCannotAddAttendees) {
					h.logger.ErrorContext(ctx, "add guest: add to calendar event", "error", err,
						"booking_id", b.ID, "host", host.UserID)
				}
			}
		}
		if attempted && !failed {
			return "calendar", nil
		}
	}

	d, err := h.loadConfirmationData(ctx, b, false)
	if err != nil {
		return "email", err
	}
	primary := primaryHost(hosts)
	d.HostName, d.HostEmail = primary.Name, primary.Email
	h.applyInviteDelivery(ctx, &d, b.InviteDelivery, b.HostID)
	// Calendar mode lands here only when no provider event invited this guest, so the
	// .ics is their invite.
	d.AttachICS = true
	d.ICSSequence = int(b.UpdatedAt.Unix())
	return "email", mailer.SendConfirmationToAttendee(ctx, h.mailer, d.ForGuest(g))
}
