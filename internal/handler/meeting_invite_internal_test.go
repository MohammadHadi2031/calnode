package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/calendar"
)

// recordingProvider is a named provider (it emails guests itself, like Google and Graph)
// that records every event written through it, and on AddMeet returns a join link.
type recordingProvider struct {
	*inviteProvider
	name, link string

	mu     sync.Mutex
	writes []recordedWrite
	// cancels records CancelEvent calls as "userID/eventID" (rollout gate, R1 acceptance 4).
	cancels []string
}

func (p *recordingProvider) CancelEvent(_ context.Context, userID, _, eventID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancels = append(p.cancels, userID+"/"+eventID)
	return nil
}

func (p *recordingProvider) cancelled() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.cancels...)
}

type recordedWrite struct {
	userID string
	params calendar.CreateEventParams
}

func (p *recordingProvider) Name() string { return p.name }
func (p *recordingProvider) CreateEvent(_ context.Context, userID string, params calendar.CreateEventParams) (string, string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writes = append(p.writes, recordedWrite{userID, params})
	link := ""
	if params.AddMeet {
		link = p.link
	}
	return fmt.Sprintf("%s-evt-%d", p.name, len(p.writes)), link, "primary", nil
}

func (p *recordingProvider) recorded() []recordedWrite {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]recordedWrite(nil), p.writes...)
}

type meetFixture struct {
	*inviteFixture
	google, microsoft *recordingProvider
	ownerID           string
}

// newMeetFixture: the owner (primary host) writes bookings to a work Microsoft account;
// a second host, host-2, has only Google. The event type "intro" is a collective on
// location_type locType with both hosts required.
func newMeetFixture(t *testing.T, locType string) *meetFixture {
	t.Helper()
	f := &meetFixture{inviteFixture: newInviteFixture(t, false)}
	f.google = &recordingProvider{inviteProvider: &inviteProvider{}, name: "google", link: "https://meet.google.com/abc-defg-hij"}
	f.microsoft = &recordingProvider{inviteProvider: &inviteProvider{}, name: "microsoft", link: "https://teams.microsoft.com/l/meetup-join/x"}
	svc := calendar.NewService(f.h.db)
	svc.Register(f.google)
	svc.Register(f.microsoft)
	f.h.SetCalendar(svc)

	if rec := f.createEventType(t, "intro", "calendar"); rec.Code != 201 {
		t.Fatalf("create event type: %d — %s", rec.Code, rec.Body.String())
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := f.h.db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := f.h.db.QueryRow(`SELECT user_id FROM event_types WHERE slug = 'intro'`).Scan(&f.ownerID); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE event_types SET routing_mode = 'collective', location_type = ?, location_value = NULL WHERE slug = 'intro'`, locType)
	exec(`UPDATE calendar_connections SET provider = 'microsoft', account_kind = 'work', account_email = ? WHERE id = 'conn-1'`, inviteHostEmail)
	exec(`INSERT INTO users (id, email, name, iana_timezone) VALUES ('host-2', 'second@gmail.example', 'Second Host', 'UTC')`)
	for day := 0; day < 7; day++ {
		exec(`INSERT INTO availability_rules (id, user_id, day_of_week, start_time, end_time) VALUES (?, 'host-2', ?, '00:00', '23:59')`,
			fmt.Sprintf("rule-h2-%d", day), day)
	}
	exec(`INSERT INTO calendar_connections
		  (id, user_id, provider, access_token_enc, calendar_id, check_conflicts, is_destination, created_at, account_email)
		VALUES ('conn-2', 'host-2', 'google', 'e', 'primary', 1, 1, '2026-01-01T00:00:00Z', 'second@gmail.example')`)
	exec(`INSERT INTO event_type_hosts (id, event_type_id, user_id, role, priority)
		SELECT 'seat-2', id, 'host-2', 'required', 1 FROM event_types WHERE slug = 'intro'`)
	return f
}

func (f *meetFixture) bookWith(t *testing.T, extra string) (int, string) {
	t.Helper()
	start := time.Now().UTC().Add(72 * time.Hour).Truncate(24 * time.Hour).Add(10 * time.Hour)
	body := fmt.Sprintf(`{"event_type_slug":"intro","start_at":%q,"name":"Guest","email":%q,"timezone":"Asia/Tehran"%s}`,
		start.Format(time.RFC3339), inviteBookerEmail, extra)
	rec := f.do(t, f.h.CreateBooking, "POST", "/v1/bookings", body)
	var b struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	if b.ID == "" {
		return rec.Code, b.Error
	}
	return rec.Code, b.ID
}

// The scenario this fork exists for: the primary host books into Outlook, the second host
// has Google, and the event is Google Meet. The Meet link is minted by the Google host,
// on ONE event that lists the booker, both guests and the Outlook host - so everyone gets
// a single invite and joins Meet without knocking - written in the booker's zone.
func TestSharedInvite_meetMintedByGoogleCoHostForEveryone(t *testing.T) {
	f := newMeetFixture(t, "google_meet")
	code, id := f.bookWith(t, `,"guests":["bob@example.com","Carol@Example.com","bob@example.com"]`)
	if code != 201 {
		t.Fatalf("create booking: %d — %s", code, id)
	}
	confirm := f.waitFor(t, inviteBookerEmail, 1)

	gw, mw := f.google.recorded(), f.microsoft.recorded()
	if len(gw) != 1 || len(mw) != 0 {
		t.Fatalf("writes: google=%d microsoft=%d; want exactly one shared Google event", len(gw), len(mw))
	}
	w := gw[0]
	if w.userID != "host-2" || !w.params.AddMeet {
		t.Errorf("event written for %q (AddMeet=%v); want host-2 minting Meet", w.userID, w.params.AddMeet)
	}
	var invited []string
	for _, g := range w.params.Guests {
		invited = append(invited, strings.ToLower(g.Email))
	}
	want := "bob@example.com,carol@example.com," + inviteHostEmail
	if w.params.OrganizerEmail != inviteBookerEmail || strings.Join(invited, ",") != want {
		t.Errorf("attendees = %s + %v; want booker + %s", w.params.OrganizerEmail, invited, want)
	}
	if w.params.TimeZone != "Asia/Tehran" {
		t.Errorf("event zone = %q; want the booker's (invite_timezone defaults to attendee)", w.params.TimeZone)
	}

	var loc string
	var invitedSeats int
	if err := f.h.db.QueryRow(`SELECT location_value FROM bookings WHERE id = ?`, id).Scan(&loc); err != nil {
		t.Fatal(err)
	}
	if err := f.h.db.QueryRow(`SELECT COUNT(*) FROM booking_hosts WHERE booking_id = ? AND user_id = ? AND invited_via_organizer = 1`, id, f.ownerID).Scan(&invitedSeats); err != nil {
		t.Fatal(err)
	}
	if loc != f.google.link || invitedSeats != 1 {
		t.Errorf("location = %q, owner invited seats = %d; want the Meet link and the Outlook host marked invited", loc, invitedSeats)
	}
	if icsOf(confirm) != "" {
		t.Error("booker's confirmation attached an .ics although Google already invited them")
	}
	if !strings.Contains(confirm.Text, f.google.link) {
		t.Error("booker's confirmation is missing the Meet link")
	}
}

// The booker may pick Teams on a Meet event type that offers the choice; Teams is then
// minted by the host with a work Microsoft account, and the choice is refused where the
// event type does not offer it.
func TestConferenceChoice(t *testing.T) {
	f := newMeetFixture(t, "google_meet")
	if code, msg := f.bookWith(t, `,"conference":"teams"`); code != 400 {
		t.Fatalf("teams on an event type without the choice: %d %s; want 400", code, msg)
	}
	if _, err := f.h.db.Exec(`UPDATE event_types SET allow_conference_choice = 1 WHERE slug = 'intro'`); err != nil {
		t.Fatal(err)
	}
	code, id := f.bookWith(t, `,"conference":"teams"`)
	if code != 201 {
		t.Fatalf("create booking: %d — %s", code, id)
	}
	f.waitFor(t, inviteBookerEmail, 1)
	mw := f.microsoft.recorded()
	if len(mw) != 1 || mw[0].userID != f.ownerID || !mw[0].params.AddMeet {
		t.Fatalf("microsoft writes = %+v; want one Teams-minting event by the owner", mw)
	}
	var locType, loc string
	if err := f.h.db.QueryRow(`SELECT location_type, location_value FROM bookings WHERE id = ?`, id).Scan(&locType, &loc); err != nil {
		t.Fatal(err)
	}
	if locType != "teams" || loc != f.microsoft.link {
		t.Errorf("booking location = %s %q; want teams with the Teams link", locType, loc)
	}
}

func TestGuests_validation(t *testing.T) {
	f := newMeetFixture(t, "google_meet")
	if _, err := f.h.db.Exec(`UPDATE event_types SET max_guests = 2 WHERE slug = 'intro'`); err != nil {
		t.Fatal(err)
	}
	if code, msg := f.bookWith(t, `,"guests":["a@example.com","b@example.com","c@example.com"]`); code != 400 || !strings.Contains(msg, "2") {
		t.Errorf("three guests over a cap of two: %d %q; want 400 naming the cap", code, msg)
	}
	if code, msg := f.bookWith(t, `,"guests":["not an email"]`); code != 400 || !strings.Contains(msg, "not an email") {
		t.Errorf("invalid guest: %d %q; want 400 naming the address", code, msg)
	}
	// The booker's own address and duplicates do not count against the cap.
	if code, msg := f.bookWith(t, `,"guests":["a@example.com","A@example.com","`+inviteBookerEmail+`","b@example.com"]`); code != 201 {
		t.Errorf("deduped guest list within the cap: %d %q; want 201", code, msg)
	}
}

// Calnode-delivered invites keep one guestless event per host, so Calnode itself sends
// each guest their invite - addressed to them, without the booker's manage link.
func TestGuests_calnodeDeliveryEmailsEachGuest(t *testing.T) {
	f := newMeetFixture(t, "google_meet")
	if _, err := f.h.db.Exec(`UPDATE server_settings SET smtp_host = 'smtp.team.example', email_from = ?, email_from_name = 'Team' WHERE id = 1`, inviteSenderEmail); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.db.Exec(`UPDATE event_types SET invite_delivery = 'calnode' WHERE slug = 'intro'`); err != nil {
		t.Fatal(err)
	}
	if code, id := f.bookWith(t, `,"guests":["bob@example.com"]`); code != 201 {
		t.Fatalf("create booking: %d — %s", code, id)
	}
	m := f.waitFor(t, "bob@example.com", 1)
	ics := icsOf(m)
	if ics == "" || !strings.Contains(ics, "mailto:"+inviteBookerEmail) || !strings.Contains(ics, "mailto:bob@example.com") {
		t.Errorf("guest invite .ics should list booker and guest:\n%s", ics)
	}
	if strings.Contains(m.Text, "/manage/") {
		t.Error("guest email carries the booker's manage link")
	}
	for _, w := range append(f.google.recorded(), f.microsoft.recorded()...) {
		if w.params.OrganizerEmail != "" || len(w.params.Guests) != 0 {
			t.Errorf("calnode-delivered event for %s invites %q + %v; want guestless", w.userID, w.params.OrganizerEmail, w.params.Guests)
		}
	}
}
