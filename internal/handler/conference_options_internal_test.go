package handler

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestEffectiveConferenceOptions(t *testing.T) {
	for _, tc := range []struct {
		stored, loc string
		want        []string
	}{
		{"", "google_meet", nil},                                        // nothing beside the default
		{"google_meet", "google_meet", nil},                             // only the default itself
		{"teams", "google_meet", []string{"google_meet", "teams"}},      // default joins, canonical order
		{"zoom, teams,bogus,teams", "teams", []string{"teams", "zoom"}}, // cleaned
		{"google_meet,teams", "zoom", []string{"google_meet", "teams", "zoom"}},
		{"google_meet,teams", "phone", nil}, // not a platform: no picker
	} {
		if got := effectiveConferenceOptions(tc.stored, tc.loc); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("effective(%q, %q) = %v; want %v", tc.stored, tc.loc, got, tc.want)
		}
	}
}

func (f *meetFixture) storedOptions(t *testing.T) string {
	t.Helper()
	var s string
	if err := f.h.db.QueryRow(`SELECT conference_options FROM event_types WHERE slug = 'intro'`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// An option is only accepted when the owner can mint it, and only checked when the save
// adds it: a stored option whose account went away must not block unrelated edits.
func TestPatchConferenceOptions(t *testing.T) {
	f := newMeetFixture(t, "google_meet") // the owner has a Microsoft work account, no Zoom
	patch := func(body string) (int, string) {
		rec := f.do(t, f.h.PatchEventType, http.MethodPatch, "/v1/event-types/intro", body)
		return rec.Code, rec.Body.String()
	}

	if code, body := patch(`{"conference_options":["zoom"]}`); code != 400 || !strings.Contains(body, "Zoom can't be offered") {
		t.Errorf("unmintable Zoom: %d %s; want 400", code, body)
	}
	if code, body := patch(`{"conference_options":["webex"]}`); code != 400 {
		t.Errorf("unknown platform: %d %s; want 400", code, body)
	}
	if code, body := patch(`{"conference_options":["teams"]}`); code != 200 {
		t.Fatalf("Teams via the owner's work account: %d %s", code, body)
	}
	if got := f.storedOptions(t); got != "teams" {
		t.Errorf("stored = %q; want teams", got)
	}

	// Already offered but no longer mintable: the editor re-submits it with every save.
	if _, err := f.h.db.Exec(`UPDATE event_types SET conference_options = 'teams,zoom' WHERE slug = 'intro'`); err != nil {
		t.Fatal(err)
	}
	if code, body := patch(`{"name":"Renamed","conference_options":["zoom","teams"]}`); code != 200 {
		t.Errorf("unchanged options blocked an unrelated edit: %d %s", code, body)
	}

	// The pre-list toggle from an older client still works: true = Meet and Teams.
	if code, body := patch(`{"conference_options":null,"allow_conference_choice":false}`); code != 200 {
		t.Fatalf("legacy off: %d %s", code, body)
	}
	if got := f.storedOptions(t); got != "" {
		t.Errorf("legacy off stored %q; want empty", got)
	}
	if code, body := patch(`{"allow_conference_choice":true}`); code != 200 {
		t.Fatalf("legacy on: %d %s", code, body)
	}
	if got := f.storedOptions(t); got != "google_meet,teams" {
		t.Errorf("legacy on stored %q; want google_meet,teams", got)
	}
}

// The booker may pick Zoom on a Meet event type that offers it; any platform the event
// type does not list is refused.
func TestConferenceChoice_zoom(t *testing.T) {
	f := newMeetFixture(t, "google_meet")
	if _, err := f.h.db.Exec(`UPDATE event_types SET conference_options = 'zoom' WHERE slug = 'intro'`); err != nil {
		t.Fatal(err)
	}
	if code, msg := f.bookWith(t, `,"conference":"teams"`); code != 400 {
		t.Fatalf("teams, not offered: %d %s; want 400", code, msg)
	}
	code, id := f.bookWith(t, `,"conference":"zoom"`)
	if code != 201 {
		t.Fatalf("create booking: %d — %s", code, id)
	}
	f.waitFor(t, inviteBookerEmail, 1)
	var locType string
	if err := f.h.db.QueryRow(`SELECT location_type FROM bookings WHERE id = ?`, id).Scan(&locType); err != nil {
		t.Fatal(err)
	}
	if locType != "zoom" {
		t.Errorf("location_type = %q; want zoom", locType)
	}
	for _, w := range append(f.google.recorded(), f.microsoft.recorded()...) {
		if w.params.AddMeet {
			t.Errorf("a calendar minted a Meet/Teams meeting for a Zoom booking (host %s)", w.userID)
		}
	}
}
