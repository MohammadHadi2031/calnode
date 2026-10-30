package gcal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/calendar"
)

// A PATCH replaces Google's whole attendee list, so adding a guest must send back the
// attendees already there - RSVP state included - plus the new one, and ask Google to
// email the invite.
func TestAddAttendees_keepsExistingAndInvites(t *testing.T) {
	var patched map[string][]map[string]any
	var patchQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/calendars/team-cal/events/ev-1") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"attendees":[
				{"email":"booker@example.com","responseStatus":"accepted"},
				{"email":"cohost@team.example","responseStatus":"needsAction"}]}`)
		case http.MethodPatch:
			patchQuery = r.URL.RawQuery
			_ = json.NewDecoder(r.Body).Decode(&patched)
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer srv.Close()
	c := newTestClient(t)
	c.apiBase = srv.URL
	saveDestinationConnection(t, c, "user-1", "primary")

	err := c.AddAttendees(context.Background(), "user-1", "team-cal", "ev-1", []calendar.Invitee{
		{Name: "New Guest", Email: "guest@client.example"},
		{Email: "BOOKER@example.com"}, // already there, any case
	})
	if err != nil {
		t.Fatalf("AddAttendees: %v", err)
	}
	if patchQuery != "sendUpdates=all" {
		t.Errorf("query = %q; want sendUpdates=all so Google emails the invite", patchQuery)
	}
	got := patched["attendees"]
	if len(got) != 3 {
		t.Fatalf("attendees = %v; want the two existing plus the new guest", got)
	}
	if got[0]["email"] != "booker@example.com" || got[0]["responseStatus"] != "accepted" {
		t.Errorf("existing attendee changed: %v", got[0])
	}
	if got[2]["email"] != "guest@client.example" || got[2]["displayName"] != "New Guest" {
		t.Errorf("new attendee = %v", got[2])
	}
}

// Adding someone who is already on the event sends nothing.
func TestAddAttendees_alreadyThereIsNoOp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			t.Error("patched an event whose attendees did not change")
		}
		_, _ = io.WriteString(w, `{"attendees":[{"email":"guest@client.example"}]}`)
	}))
	defer srv.Close()
	c := newTestClient(t)
	c.apiBase = srv.URL
	saveDestinationConnection(t, c, "user-1", "primary")
	if err := c.AddAttendees(context.Background(), "user-1", "", "ev-1",
		[]calendar.Invitee{{Email: "guest@client.example"}}); err != nil {
		t.Fatalf("AddAttendees: %v", err)
	}
}
