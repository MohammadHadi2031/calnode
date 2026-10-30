package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/calnode/calnode/internal/calendar"
)

// addingProvider is the Google-like invite provider that can also add attendees to an
// existing event, recording each call.
type addingProvider struct {
	*inviteProvider
	mu    sync.Mutex
	added []string // "eventID:email"
}

func (p *addingProvider) AddAttendees(_ context.Context, _, _, eventID string, add []calendar.Invitee) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range add {
		p.added = append(p.added, eventID+":"+a.Email)
	}
	return nil
}

func (p *addingProvider) calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.added...)
}

// postGuest adds a guest to bookingID as the fixture's host.
func (f *inviteFixture) postGuest(t *testing.T, bookingID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/bookings/"+bookingID+"/guests", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", f.apiKey)
	req.SetPathValue("id", bookingID)
	rec := httptest.NewRecorder()
	f.h.RequireAuth(f.h.AddBookingGuest)(rec, req)
	return rec
}

func (f *inviteFixture) listGuests(t *testing.T, bookingID string) []guestJSON {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/bookings/"+bookingID+"/guests", nil)
	req.Header.Set("X-API-Key", f.apiKey)
	req.SetPathValue("id", bookingID)
	rec := httptest.NewRecorder()
	f.h.RequireAuth(f.h.ListBookingGuests)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list guests: %d — %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items []guestJSON `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Items
}

const addedGuest = "colleague@booker.example"

// Calendar-invited booking: the guest goes onto the meeting's provider event, which
// sends the invite; Calnode does not email a second one.
func TestAddGuest_calendarEventInvitesThem(t *testing.T) {
	f := newInviteFixture(t, true)
	prov := &addingProvider{inviteProvider: f.cal}
	svc := calendar.NewService(f.h.db)
	svc.Register(prov)
	f.h.SetCalendar(svc)
	if rec := f.createEventType(t, "intro", "calendar"); rec.Code != http.StatusCreated {
		t.Fatalf("create event type: %d — %s", rec.Code, rec.Body.String())
	}
	id, _ := f.book(t, "intro")

	rec := f.postGuest(t, id, `{"email":" `+addedGuest+` ","name":"Colleague"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add guest: %d — %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"invited_via":"calendar"`) || strings.Contains(rec.Body.String(), "warning") {
		t.Errorf("response = %s; want invited through the calendar, no warning", rec.Body.String())
	}
	var eventID string
	_ = f.h.db.QueryRow(`SELECT external_event_id FROM booking_hosts WHERE booking_id = ?`, id).Scan(&eventID)
	if calls := prov.calls(); len(calls) != 1 || calls[0] != eventID+":"+addedGuest {
		t.Errorf("provider calls = %v; want %s added to the booking's event %s", calls, addedGuest, eventID)
	}
	if f.mail.find(addedGuest) != nil {
		t.Error("Calnode emailed the guest although the calendar invited them")
	}
	if g := f.listGuests(t, id); len(g) != 1 || g[0].Email != addedGuest || g[0].Name != "Colleague" {
		t.Errorf("guests = %+v", g)
	}

	// Duplicates and people who already attend are refused.
	for body, want := range map[string]int{
		`{"email":"` + strings.ToUpper(addedGuest) + `"}`: http.StatusConflict,
		`{"email":"` + inviteBookerEmail + `"}`:           http.StatusConflict,
		`{"email":"` + inviteHostEmail + `"}`:             http.StatusConflict,
		`{"email":"not-an-address"}`:                      http.StatusBadRequest,
	} {
		if rec := f.postGuest(t, id, body); rec.Code != want {
			t.Errorf("POST %s: %d; want %d (%s)", body, rec.Code, want, rec.Body.String())
		}
	}
}

// A provider that cannot add attendees (here: Google-like without AttendeeAdder, as CalDAV
// is) falls back to Calnode's own email with the invite attached.
func TestAddGuest_fallsBackToEmail(t *testing.T) {
	f := newInviteFixture(t, true)
	if rec := f.createEventType(t, "intro", "calendar"); rec.Code != http.StatusCreated {
		t.Fatalf("create event type: %d — %s", rec.Code, rec.Body.String())
	}
	id, _ := f.book(t, "intro")
	rec := f.postGuest(t, id, `{"email":"`+addedGuest+`"}`)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"invited_via":"email"`) {
		t.Fatalf("add guest: %d — %s", rec.Code, rec.Body.String())
	}
	m := f.mail.find(addedGuest)
	if m == nil {
		t.Fatal("the guest got no invitation")
	}
	if ics := icsOf(m); !strings.Contains(ics, "BEGIN:VEVENT") || !strings.Contains(ics, addedGuest) {
		t.Errorf("guest email has no usable invite: %q", ics)
	}
	if strings.Contains(m.Text, "/manage/") {
		t.Error("a guest's email must not carry the booker's manage link")
	}
}

// Calnode-invited booking: the guest gets Calnode's invite, with the booking's UID so later
// reschedules and cancellations update the same calendar entry.
func TestAddGuest_calnodeInvite(t *testing.T) {
	f := newInviteFixture(t, true)
	if rec := f.createEventType(t, "intro", "calnode"); rec.Code != http.StatusCreated {
		t.Fatalf("create event type: %d — %s", rec.Code, rec.Body.String())
	}
	id, _ := f.book(t, "intro")
	if rec := f.postGuest(t, id, `{"email":"`+addedGuest+`","name":"Colleague"}`); rec.Code != http.StatusCreated {
		t.Fatalf("add guest: %d — %s", rec.Code, rec.Body.String())
	}
	m := f.mail.find(addedGuest)
	if m == nil {
		t.Fatal("the guest got no invitation")
	}
	if ics := icsOf(m); !strings.Contains(ics, "UID:"+id+"@calnode") {
		t.Errorf("guest invite UID: %q; want %s@calnode", ics, id)
	}
	if !strings.Contains(m.Text, "Colleague") {
		t.Error("guest email should greet the guest by name")
	}

	// Cancelled bookings take no new guests.
	cancel := httptest.NewRequest(http.MethodPost, "/v1/bookings/"+id+"/cancel", strings.NewReader(`{}`))
	cancel.Header.Set("X-API-Key", f.apiKey)
	cancel.SetPathValue("id", id)
	f.h.RequireAuth(f.h.CancelBooking)(httptest.NewRecorder(), cancel)
	if rec := f.postGuest(t, id, `{"email":"late@booker.example"}`); rec.Code != http.StatusConflict {
		t.Errorf("guest on a cancelled booking: %d; want 409", rec.Code)
	}
	if rec := f.postGuest(t, "no-such-booking", `{"email":"x@booker.example"}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown booking: %d; want 404", rec.Code)
	}
}
