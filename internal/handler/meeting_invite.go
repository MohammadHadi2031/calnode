package handler

import (
	"context"
	"strings"
	"time"

	"github.com/calnode/calnode/internal/booking"
	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/i18n"
	"github.com/calnode/calnode/internal/mailer"
)

// Fork (wisegrid): additional guests, attendee-chosen Meet/Teams, one shared invite for
// multi-host bookings, and the invite's time zone. See migration 90001.

// maxGuestsCeiling is the hard API ceiling, matching migration 90001's CHECK on
// event_types.max_guests.
const maxGuestsCeiling = 50

// guestError is a booker-facing validation failure of the guest list (400).
type guestError struct{ msg string }

func (e *guestError) Error() string { return e.msg }

// normalizeGuests validates and dedupes the booker's guest emails: each must be a single
// valid address; repeats (case-insensitive) and the booker's own address are dropped.
// More than max distinct guests is an error rather than a silent truncation - the booker
// would otherwise believe someone was invited who was not.
func normalizeGuests(raw []string, organizerEmail string, max int, loc *i18n.Locale) ([]booking.Attendee, error) {
	seen := map[string]bool{strings.ToLower(organizerEmail): true}
	var out []booking.Attendee
	for _, r := range raw {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		email, err := normalizeBookerEmail(r)
		if err != nil {
			return nil, &guestError{loc.Tf("err_invalid_guest_email", r)}
		}
		key := strings.ToLower(email)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, booking.Attendee{Email: email})
	}
	if len(out) > max {
		return nil, &guestError{loc.Tf("err_too_many_guests", max)}
	}
	return out, nil
}

// conferenceChoice applies the booker's Meet/Teams pick to an event type that offers it.
// ok=false means the request named a platform this event type does not offer. A switch
// drops the event type's manual link: it was a link for the other platform.
func conferenceChoice(et *bookableEventType, choice string, locValue *string) bool {
	switch {
	case choice == "" || choice == et.LocationType:
		return true
	case !et.AllowConferenceChoice,
		et.LocationType != "google_meet" && et.LocationType != "teams",
		choice != "google_meet" && choice != "teams":
		return false
	}
	et.LocationType = choice
	*locValue = ""
	return true
}

// meetingPlan is who mints a booking's online meeting, through which provider.
type meetingPlan struct {
	hostID   string // "" = nobody can; the manual link (if any) is used
	provider string // "google" | "microsoft"
}

// pickMeetingHost returns the first assigned host (primary first) with a connected
// account able to mint locType's meeting - not necessarily their destination: a host who
// books into Outlook but also connected Google can mint the Meet link for everyone.
func (h *Handler) pickMeetingHost(ctx context.Context, gc *calendar.Service, hosts []assignedHost, locType string) meetingPlan {
	if gc == nil || (locType != "google_meet" && locType != "teams") {
		return meetingPlan{}
	}
	for _, host := range hosts {
		prov, err := gc.MeetingProvider(ctx, host.UserID, locType)
		if err != nil {
			h.logger.Error("meeting host lookup", "error", err, "host", host.UserID)
			continue
		}
		if prov != "" {
			return meetingPlan{hostID: host.UserID, provider: prov}
		}
	}
	return meetingPlan{}
}

// inviteTimezone resolves the event type's invite_timezone setting to an IANA zone:
// the booker's, the given host's, or UTC.
func (h *Handler) inviteTimezone(ctx context.Context, eventTypeID, bookerTZ, hostID string) string {
	var mode string
	_ = h.db.QueryRowContext(ctx, `SELECT invite_timezone FROM event_types WHERE id = ?`, eventTypeID).Scan(&mode)
	switch mode {
	case "utc":
		return "UTC"
	case "host":
		var tz string
		_ = h.db.QueryRowContext(ctx, `SELECT iana_timezone FROM users WHERE id = ?`, hostID).Scan(&tz)
		if tz != "" {
			return tz
		}
		return "UTC"
	default: // "attendee"
		if _, err := time.LoadLocation(bookerTZ); err == nil && bookerTZ != "" {
			return bookerTZ
		}
		return "UTC"
	}
}

// guestInvitees converts stored guests to provider and mailer invitees.
func guestInvitees(guests []booking.Attendee) ([]calendar.Invitee, []mailer.Invitee) {
	var ci []calendar.Invitee
	var mi []mailer.Invitee
	for _, g := range guests {
		ci = append(ci, calendar.Invitee{Name: g.Name, Email: g.Email})
		mi = append(mi, mailer.Invitee{Name: g.Name, Email: g.Email})
	}
	return ci, mi
}

// sharedEventInvitees is everyone on a shared event besides the booker: the guests, then
// every other assigned host. The organizer host is added too when the event is not on
// their destination (a Meet minted through a non-destination Google account), so it
// still reaches the calendar they actually use.
func (h *Handler) sharedEventInvitees(ctx context.Context, gc *calendar.Service, guests []booking.Attendee, hosts []assignedHost, organizerID, viaProvider string) []calendar.Invitee {
	out, _ := guestInvitees(guests)
	for _, host := range hosts {
		if host.UserID == organizerID {
			if viaProvider != "" && gc.DestinationProvider(ctx, host.UserID) != viaProvider {
				out = append(out, calendar.Invitee{Name: host.Name, Email: host.Email})
			}
			continue
		}
		out = append(out, calendar.Invitee{Name: host.Name, Email: host.Email})
	}
	return out
}

// providerInvited reports whether a booking's invites reached the booker (and guests)
// through a host's calendar provider: calendar delivery, and at least one seat holds an
// event written by a provider that emails guests itself. Decides whether Calnode must
// attach/send the .ics itself.
func (h *Handler) providerInvited(ctx context.Context, mode string, hosts []assignedHost) bool {
	gc := h.getCal()
	if gc == nil || mode == booking.InviteByCalnode {
		return false
	}
	for _, host := range hosts {
		if host.ExternalEventID == "" {
			continue
		}
		prov := host.ExternalProvider
		if prov == "" {
			prov = gc.DestinationProvider(ctx, host.UserID)
		}
		if gc.ProviderInvitesGuests(prov) {
			return true
		}
	}
	return false
}

// mailGuests sends each guest their own copy of an attendee email (see ForGuest). Only
// for bookings whose provider did not invite them already.
func (h *Handler) mailGuests(ctx context.Context, bookingID string, d mailer.BookingData, guests []mailer.Invitee, what string, send func(context.Context, mailer.Mailer, mailer.BookingData) error) {
	for _, g := range guests {
		if err := sendWithRetry(ctx, h.logger, bookingID, "guest", func() error {
			return send(ctx, h.mailer, d.ForGuest(g))
		}); err != nil {
			h.logger.Error(what+" email (guest)", "error", err, "booking_id", bookingID)
		}
	}
}

// loadGuests reads a booking's guests for the side-effect paths, logging (not failing)
// on error: a guest list that cannot be read must not stop the booker's own email.
func (h *Handler) loadGuests(ctx context.Context, bookingID string) []booking.Attendee {
	guests, err := h.bookingSvc.Guests(ctx, bookingID)
	if err != nil {
		h.logger.Error("load guests", "error", err, "booking_id", bookingID)
	}
	return guests
}

// eventDescription is the provider event's description: the booking reference, plus the
// join link when one is known up front (a minted Meet/Teams link is shown by the
// provider itself; this matters for the hosts' own copies and non-native clients).
func eventDescription(base, meetURL string) string {
	if meetURL == "" || strings.Contains(base, meetURL) {
		return base
	}
	return meetURL + "\n\n" + base
}
