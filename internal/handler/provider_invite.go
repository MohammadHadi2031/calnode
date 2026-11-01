package handler

import (
	"context"

	"github.com/calnode/calnode/internal/booking"
	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/mailer"
)

// Fork (wisegrid): Google's own invitation email shows the meeting in the organizer's
// calendar time zone, whatever zone the event is written in, so a booker abroad read the
// host's local time. Google is therefore a calendar.QuietInviter: it keeps the booker and
// guests on the event (Meet admits them without knocking, RSVPs land on it) but emails
// nobody, and Calnode sends every invite, update and cancellation itself - in the
// booker's language and time zone - as an .ics carrying the Google event's own UID,
// SEQUENCE and organizer. See docs/fork-google-invite-timezone.md.

// providerInvite is the identity of the calendar event a booking's invites belong to,
// and which host's seat holds it.
type providerInvite struct {
	calendar.EventInvite
	seat string // user id of the host whose calendar holds the event
}

// providerEventInvite returns the identity of the booking's guest-holding event when its
// provider leaves the invites to Calnode, else nil. In calendar delivery the booker is on
// exactly one event: the shared organizer event, or the single host's own. nil too when
// the lookup fails (logged): the .ics then falls back to the booking's own UID, which
// still delivers the meeting, only not tied to the provider's event. hosts must be read
// after the events were written and before any was cancelled.
func (h *Handler) providerEventInvite(ctx context.Context, b *booking.Booking, hosts []assignedHost) *providerInvite {
	gc := h.getCal()
	if gc == nil || b.InviteDelivery == booking.InviteByCalnode {
		return nil
	}
	seat := -1
	for i, host := range hosts {
		if host.ExternalEventID == "" || host.InvitedViaOrganizer {
			continue
		}
		if seat < 0 || host.IsPrimary {
			seat = i
		}
	}
	if seat < 0 {
		return nil
	}
	host := hosts[seat]
	inv, err := gc.EventInvite(ctx, host.UserID, host.ExternalCalendarID, host.ExternalEventID, host.ExternalProvider)
	if err != nil {
		h.logger.Error("read calendar event invite identity", "error", err, "booking_id", b.ID, "host", host.UserID)
		return nil
	}
	if inv == nil {
		return nil
	}
	if inv.OrganizerName == "" {
		inv.OrganizerName = host.Name
	}
	return &providerInvite{EventInvite: *inv, seat: host.UserID}
}

// apply points an email's .ics at the provider event: its UID, SEQUENCE and organizer.
// A nil receiver leaves d as it is.
func (inv *providerInvite) apply(d *mailer.BookingData) {
	if inv == nil {
		return
	}
	d.ICSUID, d.ICSSequence = inv.UID, inv.Sequence
	d.ICSOrganizerName, d.ICSOrganizerEmail = inv.OrganizerName, inv.OrganizerEmail
}

// applyHost gives a host's email the provider event's invite when they attend that event
// but their own calendar is elsewhere: a co-host of a shared Google event - or its
// organizer, when the event went through a Google account that is not their destination -
// whose bookings land in Outlook or CalDAV. Google used to email them; now Calnode does. A
// host whose destination is the same provider already sees the event, as an attendee.
// Call after applyHostInvite.
func (h *Handler) applyHostProviderInvite(ctx context.Context, d *mailer.BookingData, host assignedHost, inv *providerInvite) {
	if inv == nil || (host.UserID != inv.seat && !host.InvitedViaOrganizer) {
		return
	}
	gc := h.getCal()
	if gc == nil || gc.DestinationProvider(ctx, host.UserID) == inv.Provider {
		return
	}
	d.AttachICS = true
	d.ICSWithoutAttendee = false
	inv.apply(d)
	d.ICSHostAttendee = true
}
