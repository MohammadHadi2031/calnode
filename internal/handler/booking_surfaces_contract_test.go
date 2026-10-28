package handler

import (
	"bytes"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/i18n"
)

// TestBookingSurfacesShareStructuralHooks pins the structural contract across the
// THREE booking surfaces — book.html and manage.html (Go templates) and embed.js
// (a vanilla-JS web component). All three load the shared booking.css and implement
// the same calendar/slot-picker, but their markup is authored separately (Go
// template vs JS DOM-building), so they drift — the exact hazard CLAUDE.md warns
// about. Go template partials can't reach the JS embed, so this is the cross-language
// safety net: change the calendar/slots structure on one surface and forget another,
// and CI fails here.
//
// The pages are rendered WITHOUT the shared booking-logic.js inlined (BookingLogicJS
// left empty) on purpose: each hook must be present in the surface's OWN markup/script,
// otherwise the shared module would mask per-surface drift.
//
// Hooks that legitimately differ are deliberately excluded: the mobile step-flow uses
// .cal-back buttons in the templates vs .step-cal/.step-right card classes in the
// embed, and month nav uses #prev-btn/#next-btn in the templates vs the embed's own.
func TestBookingSurfacesShareStructuralHooks(t *testing.T) {
	var bookBuf, manageBuf bytes.Buffer
	if err := bookTmpl.Execute(&bookBuf, bookPageData{T: i18n.Default().T}); err != nil {
		t.Fatalf("book render: %v", err)
	}
	// Zero value → TokenInvalid false + Status "" (not "cancelled"), so the reschedule
	// calendar branch renders.
	if err := manageTmpl.Execute(&manageBuf, managePageData{T: i18n.Default().T}); err != nil {
		t.Fatalf("manage render: %v", err)
	}

	surfaces := map[string]string{
		"book.html":   bookBuf.String(),
		"manage.html": manageBuf.String(),
		"embed.js":    string(embedJS),
	}

	// Shared calendar/slots hooks every surface must expose — booking.css styles these
	// and the pickers depend on them. Verified present in all three when authored.
	hooks := []string{
		"cal-nav",      // month-navigation row
		"cal-grid",     // the day grid
		"month-label",  // current-month label
		"cal-col",      // calendar column
		"right-col",    // slots/form column
		"slots-list",   // slot-button container
		"slots-header", // selected-day header
		"slot-btn",     // a time-slot button
	}

	for _, h := range hooks {
		for name, src := range surfaces {
			if !strings.Contains(src, h) {
				t.Errorf("structural hook %q missing from %s — the booking surfaces have drifted; "+
					"add it to all three (book.html, manage.html, embed.js) or adjust this contract", h, name)
			}
		}
	}
}

// TestBookingSurfacesExplainEmptyDaysAndMinNotice is the same safety net for the strings
// and payload field that answer "why can't I see those times" (#20). All three surfaces
// have to name the day on an empty one, name the host when there is exactly one, and
// surface the minimum-notice policy when that is what removed the nearest starts — and
// each does it in its own separately-authored code, so forgetting one is silent.
func TestBookingSurfacesExplainEmptyDaysAndMinNotice(t *testing.T) {
	var bookBuf, manageBuf bytes.Buffer
	if err := bookTmpl.Execute(&bookBuf, bookPageData{T: i18n.Default().T}); err != nil {
		t.Fatalf("book render: %v", err)
	}
	if err := manageTmpl.Execute(&manageBuf, managePageData{T: i18n.Default().T}); err != nil {
		t.Fatalf("manage render: %v", err)
	}
	surfaces := map[string]string{
		"book.html":   bookBuf.String(),
		"manage.html": manageBuf.String(),
		"embed.js":    string(embedJS),
	}

	required := []string{
		"no_available_times",      // the empty-day message, which now names the date
		"no_available_times_host", // its "<host> has no available times on <date>" form
		"min_notice_hint",         // the minimum-notice explanation
		"min_notice",              // the GET /slots field saying which days it applied to
	}
	for _, key := range required {
		for name, src := range surfaces {
			if !strings.Contains(src, key) {
				t.Errorf("%q missing from %s — an empty or thinned day there will not explain "+
					"itself; add it to all three surfaces (#20)", key, name)
			}
		}
	}

	// Every locale must actually carry the keys the surfaces look up. i18n's own key-parity
	// test compares locales against English; this checks English has them at all, so a
	// renamed key can't leave three surfaces rendering their own key names at visitors.
	en := i18n.Default()
	for _, key := range []string{"no_available_times", "no_available_times_host", "min_notice_hint"} {
		if en.T(key) == key {
			t.Errorf("locale key %q is missing from en.json — Locale.T falls back to the key "+
				"itself, so the booking page would show %q to a visitor", key, key)
		}
	}
}

// TestEmbedJSDoesNotDependOnBookingLogic pins the trap that the shared module's own header
// used to get wrong: embed.js is served as standalone bytes (EmbedJS writes the embedded
// file unmodified), so `BookingLogic` does not exist inside the widget. It carries its own
// copies of the few helpers it needs. A well-meant de-duplication onto BookingLogic would
// throw a ReferenceError on a customer's site, where nothing here would see it.
func TestEmbedJSDoesNotDependOnBookingLogic(t *testing.T) {
	for i, line := range strings.Split(string(embedJS), "\n") {
		code := strings.TrimSpace(line)
		if strings.HasPrefix(code, "//") { // comments may reference it by name
			continue
		}
		if strings.Contains(code, "BookingLogic") {
			t.Errorf("embed.js:%d calls into BookingLogic, which is not loaded in the widget: %s",
				i+1, code)
		}
	}
}

// TestCreationSurfacesShareGuestsAndConferenceHooks pins the additional-guests field and
// the Google Meet / Microsoft Teams choice on the two surfaces that CREATE bookings:
// book.html and embed.js. Both are authored separately (Go template vs DOM-building JS)
// and styled once in booking.css, so the class names, the locale keys and the POST field
// names have to agree or one surface silently loses the feature. manage.html is left out
// on purpose: guests and the platform are chosen at creation, not on reschedule.
func TestCreationSurfacesShareGuestsAndConferenceHooks(t *testing.T) {
	var bookBuf bytes.Buffer
	// Rendered with both features on: the markup is conditional on them.
	data := bookPageData{T: i18n.Default().T, MaxGuests: 3, GuestsHint: "hint",
		ConferenceChoice: true, DefaultConference: "teams"}
	if err := bookTmpl.Execute(&bookBuf, data); err != nil {
		t.Fatalf("book render: %v", err)
	}
	surfaces := map[string]string{
		"book.html": bookBuf.String(),
		"embed.js":  string(embedJS),
	}
	hooks := []string{
		// booking.css classes
		"guests-field", "guest-list", "guest-chip", "guest-remove", "guest-add-row",
		"guest-add-btn", "guest-error", "guests-hint",
		"conference-field", "conference-options", "conference-option",
		// POST /v1/bookings fields and the radio values
		"guests:", "conference:", `"google_meet"`, `"teams"`,
	}
	for _, h := range hooks {
		for name, src := range surfaces {
			// Quote style differs (template attributes vs JS strings); compare on one.
			if !strings.Contains(strings.ReplaceAll(src, "'", `"`), h) {
				t.Errorf("guests/conference hook %q missing from %s — book.html and embed.js "+
					"have drifted; add it to both or adjust this contract", h, name)
			}
		}
	}
	// Locale keys: book.html resolves some server-side ({{call .T ...}}), so they are only
	// visible in its template source, not in the rendered page.
	sources := map[string]string{"book.html": bookTmplSrc, "embed.js": string(embedJS)}
	for _, key := range []string{"guests_label", "guests_placeholder", "guests_add", "guests_remove",
		"guests_duplicate", "guests_self", "err_invalid_guest_email",
		"conference_label", "conference_google_meet", "conference_teams"} {
		for name, src := range sources {
			if !strings.Contains(src, key) {
				t.Errorf("locale key %q not used by %s — the guests/conference copy has drifted", key, name)
			}
		}
	}
	// The widget reads these from GET /public; book.go must keep sending them.
	for _, field := range []string{"max_guests", "guests_hint", "allow_conference_choice", "default_conference"} {
		if !strings.Contains(string(embedJS), "info."+field) {
			t.Errorf("embed.js no longer reads /public field %q", field)
		}
	}
	en := i18n.Default()
	for _, key := range []string{"guests_label", "guests_placeholder", "guests_add", "guests_remove",
		"guests_hint", "guests_hint_one", "guests_duplicate", "guests_self",
		"err_invalid_guest_email", "err_too_many_guests",
		"conference_label", "conference_google_meet", "conference_teams"} {
		if en.T(key) == key {
			t.Errorf("locale key %q is missing from en.json", key)
		}
	}
}
