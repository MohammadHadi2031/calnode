package handler

// Rollout-gate tests (wisegrid fork, checklist R9): the multi-host booking flow must hold
// under a race for one slot and through 20 consecutive bookings with zero errors. Both
// run on the collective A+B fixture - an Outlook primary host and a Google co-host, Meet
// minted by the Google host - with the email log wired as in production.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/mailer"
)

// withEmailLog routes the fixture's mail through a Live mailer with the email log
// observer, as server wiring does, keeping the capture mailer as the transport.
func (f *meetFixture) withEmailLog() {
	live := mailer.NewLive(f.mail)
	live.SetObserver(f.h.EmailLogObserver())
	f.h.SetMailer(live, "https://book.team.example")
}

// bookAt books the collective slot at start for booker email, returning the status and
// the booking id or error message.
func (f *meetFixture) bookAt(t *testing.T, start time.Time, email string) (int, string) {
	t.Helper()
	body := fmt.Sprintf(`{"event_type_slug":"intro","start_at":%q,"name":"Lead","email":%q,"timezone":"Asia/Tehran"}`,
		start.Format(time.RFC3339), email)
	rec := f.do(t, f.h.CreateBooking, http.MethodPost, "/v1/bookings", body)
	var b struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	if b.ID != "" {
		return rec.Code, b.ID
	}
	return rec.Code, b.Error
}

// R1 acceptance 3: leads race for the same A+B slot. Exactly one booking succeeds; every
// other lead is told plainly that the slot is gone, and no host is double-booked.
func TestGate_collectiveRaceForOneSlot(t *testing.T) {
	f := newMeetFixture(t, "google_meet")
	start := time.Now().UTC().Add(72 * time.Hour).Truncate(24 * time.Hour).Add(10 * time.Hour)

	const n = 10
	codes, bodies := make([]int, n), make([]string, n)
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate // release together so the requests truly overlap
			codes[i], bodies[i] = f.bookAt(t, start, fmt.Sprintf("lead%d@client.example", i))
		}(i)
	}
	close(gate)
	wg.Wait()

	created := 0
	for i, c := range codes {
		switch {
		case c == http.StatusCreated:
			created++
		case c == http.StatusConflict && strings.Contains(bodies[i], "no longer available"):
		default:
			t.Errorf("lead %d: %d %q; want 201, or 409 saying the slot is no longer available", i, c, bodies[i])
		}
	}
	if created != 1 {
		t.Fatalf("%d bookings created for one slot; want exactly 1", created)
	}
	var seats int
	if err := f.h.db.QueryRow(`
		SELECT COUNT(*) FROM booking_hosts bh JOIN bookings b ON b.id = bh.booking_id
		WHERE b.status = 'confirmed' AND b.start_at = ?`, start.Format(time.RFC3339)).Scan(&seats); err != nil {
		t.Fatal(err)
	}
	if seats != 2 {
		t.Errorf("confirmed host seats at the slot = %d; want 2 (A and B, once each)", seats)
	}
}

// R9 soak: 20 consecutive bookings, zero errors. Every booking is created, gets its one
// shared calendar event with the Meet link, and its booker confirmation is logged as sent.
func TestGate_soakTwentyConsecutiveBookings(t *testing.T) {
	f := newMeetFixture(t, "google_meet")
	f.withEmailLog()
	base := time.Now().UTC().Add(48 * time.Hour).Truncate(24 * time.Hour).Add(8 * time.Hour)

	const n = 20
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		// Hourly slots over consecutive days: never adjacent to each other's buffers.
		start := base.Add(time.Duration(i/8) * 24 * time.Hour).Add(time.Duration(i%8) * time.Hour)
		code, id := f.bookAt(t, start, fmt.Sprintf("soak%d@client.example", i))
		if code != http.StatusCreated {
			t.Fatalf("booking %d at %s: %d %s", i+1, start.Format(time.RFC3339), code, id)
		}
		ids = append(ids, id)
	}

	// Confirmations go out in the background; wait for all of them.
	deadline := time.Now().Add(15 * time.Second)
	for {
		var sent, bad int
		_ = f.h.db.QueryRow(`SELECT
			COUNT(*) FILTER (WHERE role = 'attendee' AND kind = 'confirmation' AND status = 'sent'),
			COUNT(*) FILTER (WHERE status IN ('failed', 'bounced'))
			FROM email_log`).Scan(&sent, &bad)
		if bad > 0 {
			t.Fatalf("%d email attempts failed during the soak", bad)
		}
		if sent == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("booker confirmations sent = %d; want %d", sent, n)
		}
		time.Sleep(20 * time.Millisecond)
	}

	for _, id := range ids {
		var loc string
		var failed, events int
		if err := f.h.db.QueryRow(`SELECT location_value, confirm_failed FROM bookings WHERE id = ?`, id).Scan(&loc, &failed); err != nil {
			t.Fatal(err)
		}
		_ = f.h.db.QueryRow(`SELECT COUNT(*) FROM booking_hosts WHERE booking_id = ? AND external_event_id != ''`, id).Scan(&events)
		if loc != "https://meet.google.com/abc-defg-hij" || failed != 0 || events != 1 {
			t.Errorf("booking %s: location %q, confirm_failed %d, provider events %d; want the Meet link, 0, one shared event",
				id, loc, failed, events)
		}
	}
	if w := len(f.google.recorded()); w != n {
		t.Errorf("Google events written = %d; want %d (one shared event per booking)", w, n)
	}
}
