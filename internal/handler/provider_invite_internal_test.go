package handler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/booking"
	"github.com/calnode/calnode/internal/calendar"
)

// quietProvider is Google as the fork drives it: the booker goes on the event, but the
// provider emails nobody and hands out the event's iCalendar identity instead.
type quietProvider struct {
	*inviteProvider
	seq int
}

func (p *quietProvider) InvitesGuests() bool { return false }
func (p *quietProvider) UpdateEvent(context.Context, string, string, string, time.Time, time.Time) error {
	p.seq++ // Google bumps SEQUENCE when an event's time changes
	return nil
}
func (p *quietProvider) EventInvite(_ context.Context, _, _, eventID string) (calendar.EventInvite, error) {
	return calendar.EventInvite{
		UID: eventID + "@google.com", Sequence: p.seq,
		OrganizerName: "Jeroen Host", OrganizerEmail: "jeroen@gmail.example",
	}, nil
}

// Google's own invitation shows the organizer's time zone, so with Google silenced the
// booker's invite, its update and its cancellation all come from Calnode - each an .ics
// that IS the Google event (same UID, its SEQUENCE, its organizer) - while the booker stays
// on that event as an attendee.
func TestProviderInvite_quietGoogleEventInvitedByCalnode(t *testing.T) {
	f := newInviteFixture(t, true)
	q := &quietProvider{inviteProvider: f.cal}
	svc := calendar.NewService(f.h.db)
	svc.Register(q)
	f.h.SetCalendar(svc)
	if rec := f.createEventType(t, "intro", booking.InviteByCalendar); rec.Code != 201 {
		t.Fatalf("create event type: %d — %s", rec.Code, rec.Body.String())
	}
	id, msg := f.book(t, "intro")

	events := f.cal.events()
	if len(events) != 1 || events[0].OrganizerEmail != inviteBookerEmail {
		t.Fatalf("host calendar event guests = %+v; want the booker kept on the Google event", events)
	}
	ics := icsOf(msg)
	for _, want := range []string{
		"METHOD:REQUEST", "UID:evt-1@google.com", "SEQUENCE:0",
		`ORGANIZER;CN="Jeroen Host":mailto:jeroen@gmail.example`, "mailto:" + inviteBookerEmail,
	} {
		if !strings.Contains(ics, want) {
			t.Errorf("confirmation .ics missing %q:\n%s", want, ics)
		}
	}

	ctx := context.Background()
	b, err := f.h.bookingSvc.Get(ctx, id)
	if err != nil {
		t.Fatalf("get booking: %v", err)
	}
	var etID string
	_ = f.h.db.QueryRow(`SELECT event_type_id FROM bookings WHERE id = ?`, id).Scan(&etID)
	newStart := b.StartAt.Add(24 * time.Hour)
	updated, err := f.h.bookingSvc.Reschedule(ctx, id, newStart, newStart.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("reschedule: %v", err)
	}
	f.h.rescheduleSideEffects(*updated, etID, b.StartAt, b.EndAt)
	ics = icsOf(f.waitFor(t, inviteBookerEmail, 2))
	for _, want := range []string{"METHOD:REQUEST", "UID:evt-1@google.com", "SEQUENCE:1",
		"DTSTART:" + newStart.UTC().Format("20060102T150405Z")} {
		if !strings.Contains(ics, want) {
			t.Errorf("reschedule .ics missing %q:\n%s", want, ics)
		}
	}

	if err := f.h.bookingSvc.CancelByID(ctx, id, "changed plans"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	cancelled, err := f.h.bookingSvc.Get(ctx, id)
	if err != nil {
		t.Fatalf("get booking: %v", err)
	}
	f.h.cancelSideEffects(*cancelled)
	ics = icsOf(f.waitFor(t, inviteBookerEmail, 3))
	for _, want := range []string{"METHOD:CANCEL", "UID:evt-1@google.com"} {
		if !strings.Contains(ics, want) {
			t.Errorf("cancellation .ics missing %q:\n%s", want, ics)
		}
	}

	// The host's own copies never carry an .ics: the event is on their Google calendar.
	f.mail.mu.Lock()
	defer f.mail.mu.Unlock()
	for _, m := range f.mail.msgs {
		if len(m.To) > 0 && m.To[0] == inviteHostEmail {
			if ics := icsOf(&m); ics != "" {
				t.Errorf("host email %q has an .ics for an event already on their calendar:\n%s", m.Subject, ics)
			}
		}
	}
}

// quietRecording is the meet fixture's Google with the fork's behaviour: no emails, its
// events' identity handed to Calnode instead.
type quietRecording struct{ *recordingProvider }

func (p quietRecording) InvitesGuests() bool { return false }
func (p quietRecording) EventInvite(_ context.Context, _, _, eventID string) (calendar.EventInvite, error) {
	return calendar.EventInvite{UID: eventID + "@google.com", OrganizerEmail: "second@gmail.example"}, nil
}

// The fork's main scenario with Google silenced: one shared Google Meet event on the
// Google co-host's calendar lists the booker, the guests and the Outlook host. Calnode
// then invites each of them with that event's .ics - the Outlook host included, whom
// Google used to email - while the Google host, whose calendar holds it, gets none.
func TestProviderInvite_sharedGoogleEventReachesEveryoneFromCalnode(t *testing.T) {
	f := newMeetFixture(t, "google_meet")
	svc := calendar.NewService(f.h.db)
	svc.Register(quietRecording{f.google})
	svc.Register(f.microsoft)
	f.h.SetCalendar(svc)

	code, id := f.bookWith(t, `,"guests":["bob@example.com"]`)
	if code != 201 {
		t.Fatalf("create booking: %d — %s", code, id)
	}
	f.waitFor(t, inviteBookerEmail, 1) // the calendar phase is done once it is sent
	if gw := f.google.recorded(); len(gw) != 1 || gw[0].params.OrganizerEmail != inviteBookerEmail {
		t.Fatalf("google writes = %+v; want the one shared event with the booker on it", gw)
	}
	const uid = "UID:google-evt-1@google.com"
	for _, who := range []string{inviteBookerEmail, "bob@example.com", inviteHostEmail} {
		ics := icsOf(f.waitFor(t, who, 1))
		if !strings.Contains(ics, uid) || !strings.Contains(ics, "mailto:second@gmail.example") {
			t.Errorf("%s: .ics is not the shared Google event's invite:\n%s", who, ics)
		}
	}
	if ics := icsOf(f.waitFor(t, inviteHostEmail, 1)); !strings.Contains(ics, "ATTENDEE;CN=\"Jeroen Host\";RSVP=TRUE:mailto:"+inviteHostEmail) {
		t.Errorf("Outlook host's copy does not list them as an attendee:\n%s", ics)
	}
	if ics := icsOf(f.waitFor(t, "second@gmail.example", 1)); ics != "" {
		t.Errorf("Google host got an .ics for the event on their own calendar:\n%s", ics)
	}
}
