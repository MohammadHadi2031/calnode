package mailer

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Checklist R5: every email names its zone, and shows a second one, so nobody converts by
// hand. The lead reads their own zone with UTC; a host reads their own zone with the lead's.
func TestEmailTimes_zoneNamesAndSecondZone(t *testing.T) {
	start := time.Date(2026, 10, 1, 16, 0, 0, 0, time.UTC) // 19:30 in Tehran
	d := BookingData{StartAt: start, EndAt: start.Add(30 * time.Minute), OrganizerTimezone: "Asia/Tehran"}

	if got := d.StartFmt(); !strings.Contains(got, "7:30 PM Tehran") || !strings.Contains(got, "(4:00 PM UTC)") {
		t.Errorf("lead StartFmt = %q; want Tehran time with UTC alongside", got)
	}
	if got := d.WhenFmt(); !strings.HasSuffix(got, "7:30 PM – 8:00 PM Tehran (4:00 PM – 4:30 PM UTC)") {
		t.Errorf("lead WhenFmt = %q", got)
	}

	// The host's own copy: Istanbul first, the lead's Tehran alongside.
	host := d
	host.HostTimezone, host.hostView = "Europe/Istanbul", true
	if got := host.WhenFmt(); !strings.HasSuffix(got, "7:00 PM – 7:30 PM Istanbul (7:30 PM – 8:00 PM Tehran)") {
		t.Errorf("host WhenFmt = %q", got)
	}
	// A host with no zone on file reads the lead's, with UTC.
	host.HostTimezone = ""
	if got := host.StartFmt(); !strings.Contains(got, "Tehran (4:00 PM UTC)") {
		t.Errorf("host without zone StartFmt = %q", got)
	}

	// No second zone when it would only repeat the first.
	utc := BookingData{StartAt: start, EndAt: start.Add(time.Hour), OrganizerTimezone: "UTC"}
	if got := utc.StartFmt(); strings.Contains(got, "(") || !strings.HasSuffix(got, "4:00 PM UTC") {
		t.Errorf("UTC lead StartFmt = %q; want one zone", got)
	}
	amman := host
	amman.OrganizerTimezone, amman.HostTimezone = "Asia/Amman", "Europe/Istanbul" // both +03
	if got := amman.StartFmt(); strings.Contains(got, "(") {
		t.Errorf("same-offset zones StartFmt = %q; want no repeat", got)
	}

	// Unknown zone names fall back to UTC instead of failing the email.
	bad := BookingData{StartAt: start, EndAt: start, OrganizerTimezone: "Mars/Olympus"}
	if got := bad.StartFmt(); !strings.HasSuffix(got, "4:00 PM UTC") {
		t.Errorf("unknown zone StartFmt = %q", got)
	}
}

// When the second zone is on another day, it carries its own date.
func TestEmailTimes_secondZoneOnAnotherDay(t *testing.T) {
	start := time.Date(2026, 10, 1, 22, 0, 0, 0, time.UTC) // Fri 2 Oct, 11:00 in Auckland
	d := BookingData{StartAt: start, EndAt: start.Add(time.Hour), OrganizerTimezone: "Pacific/Auckland"}
	got := d.WhenFmt()
	if !strings.Contains(got, "2 Oct 2026, 11:00 AM") || !strings.Contains(got, "(Thu 1 Oct 2026, 10:00 PM – 11:00 PM UTC)") {
		t.Errorf("WhenFmt = %q; want the UTC side dated Thu 1 Oct", got)
	}
}

// DST: the same 10:00 UTC meeting is 12:00 in Berlin before the 25 Oct 2026 change and
// 11:00 after it. Each instant uses its own offset, whenever the booking was made.
func TestEmailTimes_daylightSaving(t *testing.T) {
	before := BookingData{OrganizerTimezone: "Europe/Berlin",
		StartAt: time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)}
	after := before
	after.StartAt = time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	if got := before.StartFmt(); !strings.Contains(got, "12:00 PM Berlin (10:00 AM UTC)") {
		t.Errorf("before the change: %q", got)
	}
	if got := after.StartFmt(); !strings.Contains(got, "11:00 AM Berlin (10:00 AM UTC)") {
		t.Errorf("after the change: %q", got)
	}
	// A reschedule across the change shows each time in its own offset.
	moved := after
	moved.PreviousStartAt = before.StartAt
	if p, n := moved.PreviousStartFmt(), moved.StartFmt(); !strings.Contains(p, "12:00 PM Berlin") || !strings.Contains(n, "11:00 AM Berlin") {
		t.Errorf("reschedule across DST: previous %q, new %q", p, n)
	}
}

// The host's email really is written in the host's zone: the flag is set by the host
// senders, never by the attendee ones.
func TestEmailTimes_hostSendersUseHostZone(t *testing.T) {
	start := time.Date(2026, 10, 1, 16, 0, 0, 0, time.UTC)
	d := BookingData{BookingID: "b1", EventTypeName: "Demo", OrganizerName: "Lead", OrganizerEmail: "lead@x.example",
		OrganizerTimezone: "Asia/Tehran", HostName: "Host", HostEmail: "host@x.example", HostTimezone: "America/New_York",
		StartAt: start, EndAt: start.Add(30 * time.Minute)}
	m := &recordMailer{}
	if err := SendConfirmationToHost(context.Background(), m, d); err != nil {
		t.Fatal(err)
	}
	if err := SendConfirmationToAttendee(context.Background(), m, d); err != nil {
		t.Fatal(err)
	}
	if len(m.msgs) != 2 {
		t.Fatalf("sent %d", len(m.msgs))
	}
	if body := m.msgs[0].Text; !strings.Contains(body, "12:00 PM New York (7:30 PM Tehran)") {
		t.Errorf("host email times:\n%s", body)
	}
	if body := m.msgs[1].Text; !strings.Contains(body, "7:30 PM Tehran (4:00 PM UTC)") || strings.Contains(body, "New York") {
		t.Errorf("lead email times:\n%s", body)
	}
}

type recordMailer struct{ msgs []Message }

func (r *recordMailer) Send(_ context.Context, m Message) error {
	r.msgs = append(r.msgs, m)
	return nil
}
