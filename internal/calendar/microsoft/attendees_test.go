package microsoft

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

// Graph replaces the attendee list on PATCH: the existing attendees go back with the new
// guest, and Graph sends the invite.
func TestAddAttendees_keepsExisting(t *testing.T) {
	c := newTestClient(t)
	connect(t, c, "u1")
	var patched struct {
		Attendees []graphAttendee `json:"attendees"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/me/events/evt-1") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"attendees":[{"emailAddress":{"address":"booker@example.com","name":"Booker"},"type":"required"}]}`)
		case http.MethodPatch:
			_ = json.NewDecoder(r.Body).Decode(&patched)
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer srv.Close()
	c.apiBase = srv.URL

	if err := c.AddAttendees(context.Background(), "u1", "", "evt-1",
		[]calendar.Invitee{{Name: "Guest", Email: "guest@client.example"}}); err != nil {
		t.Fatalf("AddAttendees: %v", err)
	}
	if len(patched.Attendees) != 2 ||
		patched.Attendees[0].EmailAddress.Address != "booker@example.com" ||
		patched.Attendees[1].EmailAddress.Address != "guest@client.example" ||
		patched.Attendees[1].Type != "required" {
		t.Errorf("patched attendees = %+v; want the booker kept and the guest added", patched.Attendees)
	}
}
