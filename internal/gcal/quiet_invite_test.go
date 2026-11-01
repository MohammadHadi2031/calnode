package gcal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/calendar"
)

// Google must email nobody on any write: its invitation shows the organizer's time zone,
// so Calnode sends every invite, update and cancellation itself (wisegrid fork).
func TestWrites_neverAskGoogleToEmail(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.Method+" "+r.URL.Query().Get("sendUpdates"))
		switch r.Method {
		case http.MethodPost:
			_, _ = io.WriteString(w, `{"id":"ev-1"}`)
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer srv.Close()
	c := newTestClient(t)
	c.apiBase = srv.URL
	saveDestinationConnection(t, c, "user-1", "primary")
	ctx := context.Background()
	start := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)

	if _, _, _, err := c.CreateEvent(ctx, "user-1", calendar.CreateEventParams{
		Summary: "s", Start: start, End: start.Add(30 * time.Minute), OrganizerEmail: "booker@example.com",
	}); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	if err := c.UpdateEvent(ctx, "user-1", "", "ev-1", start.Add(time.Hour), start.Add(90*time.Minute)); err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}
	if err := c.CancelEvent(ctx, "user-1", "", "ev-1"); err != nil {
		t.Fatalf("CancelEvent: %v", err)
	}
	if len(queries) != 3 {
		t.Fatalf("calls = %v; want create, update, cancel", queries)
	}
	for _, q := range queries {
		if q[len(q)-4:] != "none" {
			t.Errorf("%s; want sendUpdates=none on every write", q)
		}
	}
	if c.InvitesGuests() {
		t.Error("InvitesGuests() = true; Google emails nobody, Calnode must attach the .ics")
	}
}

// EventInvite reads the identity Calnode's .ics must carry to BE the Google event.
func TestEventInvite_readsIdentity(t *testing.T) {
	var path, fields string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, fields = r.URL.Path, r.URL.Query().Get("fields")
		_, _ = io.WriteString(w, `{"iCalUID":"abc123@google.com","sequence":2,
			"organizer":{"email":"host@team.example","displayName":"Host Name"}}`)
	}))
	defer srv.Close()
	c := newTestClient(t)
	c.apiBase = srv.URL
	saveDestinationConnection(t, c, "user-1", "primary")

	inv, err := c.EventInvite(context.Background(), "user-1", "team-cal", "ev-1")
	if err != nil {
		t.Fatalf("EventInvite: %v", err)
	}
	if path != "/calendars/team-cal/events/ev-1" {
		t.Errorf("path = %q; want the recorded calendar's event", path)
	}
	if fields == "" {
		t.Error("no fields filter; want only the identity read")
	}
	want := calendar.EventInvite{UID: "abc123@google.com", Sequence: 2, OrganizerName: "Host Name", OrganizerEmail: "host@team.example"}
	if inv != want {
		t.Errorf("invite = %+v; want %+v", inv, want)
	}
}

// A reassigned booking's new event takes over the old one's iCalUID; if Google refuses
// it, the event is still created, under its own identity.
func TestCreateEvent_iCalUIDRefusedRetriesWithout(t *testing.T) {
	var bodies []calEventReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req calEventReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		bodies = append(bodies, req)
		if req.ICalUID != "" {
			w.WriteHeader(http.StatusConflict)
			return
		}
		_, _ = io.WriteString(w, `{"id":"ev-2"}`)
	}))
	defer srv.Close()
	c := newTestClient(t)
	c.apiBase = srv.URL
	saveDestinationConnection(t, c, "user-1", "primary")
	start := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)

	id, _, _, err := c.CreateEvent(context.Background(), "user-1", calendar.CreateEventParams{
		Summary: "s", Start: start, End: start.Add(30 * time.Minute),
		ICalUID: "old@google.com", Sequence: 3,
	})
	if err != nil || id != "ev-2" {
		t.Fatalf("CreateEvent = %q, %v; want ev-2 from the retry", id, err)
	}
	if len(bodies) != 2 || bodies[0].ICalUID != "old@google.com" || bodies[0].Sequence != 3 || bodies[1].ICalUID != "" {
		t.Errorf("requests = %+v; want one with the old identity, then one without", bodies)
	}
}
