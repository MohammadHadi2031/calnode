package handler_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/handler"
)

// guestsConferenceFixture is one event type's stored settings for additional guests and
// the booker's conference choice, written straight to the row (the admin API for these
// columns is a separate change) so the public surfaces can be checked against each case.
type guestsConferenceFixture struct {
	locType   string
	maxGuests int
	allowConf bool
}

func seedGuestsConference(t *testing.T, fx guestsConferenceFixture) (*handler.Handler, string) {
	t.Helper()
	h, db, key, _ := setupWorkspaceWithDB(t)
	slug, id := seedEventTypeHTTP(t, h, key)
	setGuestsConference(t, db, id, fx)
	return h, slug
}

func setGuestsConference(t *testing.T, db *sql.DB, id string, fx guestsConferenceFixture) {
	t.Helper()
	if _, err := db.Exec(`UPDATE event_types SET location_type = ?, location_value = 'https://example.com/room',
		max_guests = ?, allow_conference_choice = ? WHERE id = ?`,
		fx.locType, fx.maxGuests, fx.allowConf, id); err != nil {
		t.Fatal(err)
	}
}

func fetchPublic(t *testing.T, h *handler.Handler, slug string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/event-types/"+slug+"/public", nil)
	req.SetPathValue("slug", slug)
	rec := httptest.NewRecorder()
	h.PublicEventType(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("public: %d — %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func renderBookPage(t *testing.T, h *handler.Handler, slug string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/book/"+slug, nil)
	req.SetPathValue("slug", slug)
	rec := httptest.NewRecorder()
	h.BookPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("BookPage: %d — %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// The column defaults (max_guests 10, no conference choice) are what every existing event
// type gets from the migration, so they are what a booker sees until an admin changes them.
func TestPublicEventType_guestsAndConferenceDefaults(t *testing.T) {
	h, apiKey, _ := setupWorkspace(t)
	slug, _ := seedEventTypeHTTP(t, h, apiKey)

	resp := fetchPublic(t, h, slug)
	if got, _ := resp["max_guests"].(float64); got != 10 {
		t.Errorf("max_guests = %v; want the column default 10", resp["max_guests"])
	}
	if got := resp["guests_hint"]; got != "Up to 10 guests. They'll receive the calendar invite." {
		t.Errorf("guests_hint = %q", got)
	}
	if got := resp["allow_conference_choice"]; got != false {
		t.Errorf("allow_conference_choice = %v; want false by default", got)
	}
	if got := resp["default_conference"]; got != "" {
		t.Errorf("default_conference = %q; want empty with no choice offered", got)
	}
}

func TestPublicEventType_guestsAndConference(t *testing.T) {
	cases := []struct {
		name       string
		fx         guestsConferenceFixture
		wantChoice bool
		wantDef    string
		wantHint   string
	}{
		{"meet with choice", guestsConferenceFixture{"google_meet", 3, true}, true, "google_meet",
			"Up to 3 guests. They'll receive the calendar invite."},
		{"teams with choice", guestsConferenceFixture{"teams", 1, true}, true, "teams",
			"Up to 1 guest. They'll receive the calendar invite."},
		{"meet without choice", guestsConferenceFixture{"google_meet", 50, false}, false, "",
			"Up to 50 guests. They'll receive the calendar invite."},
		// The toggle only means something for a Meet/Teams event type: on Zoom (or any
		// other location) it is ignored rather than offering a platform switch.
		{"zoom ignores the toggle", guestsConferenceFixture{"zoom", 0, true}, false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, slug := seedGuestsConference(t, tc.fx)
			resp := fetchPublic(t, h, slug)
			if got, _ := resp["max_guests"].(float64); int(got) != tc.fx.maxGuests {
				t.Errorf("max_guests = %v; want %d", resp["max_guests"], tc.fx.maxGuests)
			}
			if got := resp["allow_conference_choice"]; got != tc.wantChoice {
				t.Errorf("allow_conference_choice = %v; want %v", got, tc.wantChoice)
			}
			if got := resp["default_conference"]; got != tc.wantDef {
				t.Errorf("default_conference = %q; want %q", got, tc.wantDef)
			}
			if got := resp["guests_hint"]; got != tc.wantHint {
				t.Errorf("guests_hint = %q; want %q", got, tc.wantHint)
			}
		})
	}
}

// guests_hint is composed server-side in the resolved locale, so the widget renders it as
// given instead of pluralising "%d guests" itself.
func TestPublicEventType_guestsHintIsTranslated(t *testing.T) {
	h, slug := seedGuestsConference(t, guestsConferenceFixture{"google_meet", 4, false})
	req := httptest.NewRequest(http.MethodGet, "/v1/event-types/"+slug+"/public?lang=de", nil)
	req.SetPathValue("slug", slug)
	rec := httptest.NewRecorder()
	h.PublicEventType(rec, req)
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp["guests_hint"]; got != "Bis zu 4 Gäste. Sie erhalten die Kalendereinladung." {
		t.Errorf("guests_hint (de) = %q", got)
	}
}

func TestBookPage_guestField(t *testing.T) {
	t.Run("rendered with the server-composed hint", func(t *testing.T) {
		h, slug := seedGuestsConference(t, guestsConferenceFixture{"google_meet", 2, false})
		body := renderBookPage(t, h, slug)
		for _, want := range []string{`id="f-guest-input"`, `id="guest-add-btn"`, `id="guest-list"`,
			"Up to 2 guests. They&#39;ll receive the calendar invite."} {
			if !strings.Contains(body, want) {
				t.Errorf("book page missing %q", want)
			}
		}
		// html/template pads numbers in a JS context with spaces, so compare fields.
		got := "(not found)"
		for _, line := range strings.Split(body, "\n") {
			if strings.Contains(line, "const MAX_GUESTS") {
				if f := strings.Fields(line); len(f) >= 4 {
					got = strings.TrimSuffix(f[3], ";")
				}
				break
			}
		}
		if got != "2" {
			t.Errorf("MAX_GUESTS = %s; want the cap 2", got)
		}
	})
	t.Run("absent when max_guests is 0", func(t *testing.T) {
		h, slug := seedGuestsConference(t, guestsConferenceFixture{"google_meet", 0, false})
		if body := renderBookPage(t, h, slug); strings.Contains(body, `id="f-guest-input"`) {
			t.Error("guest field rendered although the event type takes no guests")
		}
	})
}

func TestBookPage_conferenceChoice(t *testing.T) {
	cases := []struct {
		name    string
		fx      guestsConferenceFixture
		shown   bool
		checked string // the radio value expected to carry `checked`
	}{
		{"meet preselects meet", guestsConferenceFixture{"google_meet", 0, true}, true, "google_meet"},
		{"teams preselects teams", guestsConferenceFixture{"teams", 0, true}, true, "teams"},
		{"toggle off", guestsConferenceFixture{"teams", 0, false}, false, ""},
		{"not a meet/teams type", guestsConferenceFixture{"zoom", 0, true}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, slug := seedGuestsConference(t, tc.fx)
			body := renderBookPage(t, h, slug)
			shown := strings.Contains(body, `name="conference"`)
			if shown != tc.shown {
				t.Fatalf("conference choice shown = %v; want %v", shown, tc.shown)
			}
			if !tc.shown {
				return
			}
			for _, v := range []string{"google_meet", "teams"} {
				radio := `value="` + v + `"`
				i := strings.Index(body, radio)
				if i < 0 {
					t.Fatalf("radio %s missing", v)
				}
				end := strings.Index(body[i:], ">")
				isChecked := strings.Contains(body[i:i+end], "checked")
				if isChecked != (v == tc.checked) {
					t.Errorf("radio %s checked = %v; want %v", v, isChecked, v == tc.checked)
				}
			}
			for _, label := range []string{"Video conferencing", "Google Meet", "Microsoft Teams"} {
				if !strings.Contains(body, label) {
					t.Errorf("conference choice missing label %q", label)
				}
			}
		})
	}
}
