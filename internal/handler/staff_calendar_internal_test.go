package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// staffFixture: the workspace owner (a host with a Google-like destination and an "intro"
// event type, 30 min, available all day), a secretary and an unrelated member.
type staffFixture struct {
	*inviteFixture
	owner, secretary, member AuthUser
	etID                     string
	day                      time.Time // a UTC midnight three days out
}

func newStaffFixture(t *testing.T) *staffFixture {
	t.Helper()
	f := &staffFixture{inviteFixture: newInviteFixture(t, false)}
	if rec := f.createEventType(t, "intro", "calendar"); rec.Code != http.StatusCreated {
		t.Fatalf("create event type: %d — %s", rec.Code, rec.Body.String())
	}
	if err := f.h.db.QueryRow(`SELECT id, user_id FROM event_types WHERE slug = 'intro'`).Scan(&f.etID, &f.owner.ID); err != nil {
		t.Fatal(err)
	}
	f.owner.IsAdmin, f.owner.IsOwner, f.owner.Name, f.owner.Email = true, true, "Jeroen Host", inviteHostEmail
	f.secretary = AuthUser{ID: "sec-1", Name: "Sam Secretary", Email: "sam@team.example"}
	f.member = AuthUser{ID: "mem-1", Name: "Mo Member", Email: "mo@team.example"}
	for _, u := range []AuthUser{f.secretary, f.member} {
		if _, err := f.h.db.Exec(`INSERT INTO users (id, email, name, iana_timezone) VALUES (?, ?, ?, 'UTC')`, u.ID, u.Email, u.Name); err != nil {
			t.Fatal(err)
		}
	}
	f.day = time.Now().UTC().Add(72 * time.Hour).Truncate(24 * time.Hour)
	return f
}

// as calls fn as user (bypassing RequireAuth), with {id} set to pathID.
func (f *staffFixture) as(t *testing.T, user AuthUser, fn http.HandlerFunc, method, path, body, pathID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if pathID != "" {
		req.SetPathValue("id", pathID)
	}
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, user))
	rec := httptest.NewRecorder()
	fn(rec, req)
	return rec
}

func (f *staffFixture) at(hhmm string) string {
	var h, m int
	fmt.Sscanf(hhmm, "%d:%d", &h, &m)
	return f.day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute).Format(time.RFC3339)
}

func conflictKinds(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var body struct {
		Error     string          `json:"error"`
		Conflicts []staffConflict `json:"conflicts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error != "conflicts" {
		t.Fatalf("want a conflicts body, got %d %s", rec.Code, rec.Body.String())
	}
	var kinds []string
	for _, c := range body.Conflicts {
		kinds = append(kinds, c.Kind)
	}
	return kinds
}

func idOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.ID == "" {
		t.Fatalf("no id in %d %s", rec.Code, rec.Body.String())
	}
	return v.ID
}

func (f *staffFixture) publicBook(t *testing.T, start string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/bookings", strings.NewReader(fmt.Sprintf(
		`{"event_type_slug":"intro","start_at":%q,"name":"Guest","email":%q,"timezone":"UTC"}`, start, inviteBookerEmail)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.h.CreateBooking(rec, req)
	return rec.Code
}

// A secretary manages exactly the hosts an admin assigned them; a plain member manages
// only themselves; only an admin edits the assignments.
func TestSecretary_managesOnlyAssignedHosts(t *testing.T) {
	f := newStaffFixture(t)
	block := fmt.Sprintf(`{"host_id":%q,"start_at":%q,"end_at":%q,"sync_to_calendar":false}`, f.owner.ID, f.at("09:00"), f.at("10:00"))

	if rec := f.as(t, f.secretary, f.h.CreateUnavailableBlock, "POST", "/v1/unavailable-blocks", block, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("unassigned secretary: %d; want 403", rec.Code)
	}
	assign := fmt.Sprintf(`{"host_ids":[%q,%q,%q]}`, f.owner.ID, f.secretary.ID, f.owner.ID)
	if rec := f.as(t, f.member, f.h.SetSecretaryHosts, "PUT", "/v1/users/sec-1/secretary-hosts", assign, f.secretary.ID); rec.Code != http.StatusForbidden {
		t.Fatalf("member assigning: %d; want 403", rec.Code)
	}
	rec := f.as(t, f.owner, f.h.SetSecretaryHosts, "PUT", "/v1/users/sec-1/secretary-hosts", assign, f.secretary.ID)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"host_ids":["`+f.owner.ID+`"]`) {
		t.Fatalf("admin assigning: %d %s; want the owner only (self and duplicates dropped)", rec.Code, rec.Body.String())
	}

	if rec := f.as(t, f.secretary, f.h.CreateUnavailableBlock, "POST", "/v1/unavailable-blocks", block, ""); rec.Code != http.StatusCreated {
		t.Fatalf("assigned secretary: %d %s; want 201", rec.Code, rec.Body.String())
	}
	rec = f.as(t, f.secretary, f.h.ListStaffHosts, "GET", "/v1/staff/hosts", "", "")
	var hosts struct {
		Hosts []struct {
			ID          string           `json:"id"`
			Self        bool             `json:"self"`
			HasCalendar bool             `json:"has_calendar"`
			EventTypes  []staffEventType `json:"event_types"`
		} `json:"hosts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &hosts); err != nil || len(hosts.Hosts) != 2 ||
		!hosts.Hosts[0].Self || hosts.Hosts[1].ID != f.owner.ID || !hosts.Hosts[1].HasCalendar || len(hosts.Hosts[1].EventTypes) != 1 {
		t.Fatalf("secretary's hosts = %s; want self, then the owner with a calendar and the intro type", rec.Body.String())
	}

	calURL := "/v1/staff/calendar?from=" + f.at("00:00") + "&to=" + f.day.Add(24*time.Hour).Format(time.RFC3339) + "&host_ids=" + f.owner.ID
	if rec := f.as(t, f.member, f.h.GetStaffCalendar, "GET", calURL, "", ""); rec.Code != http.StatusForbidden {
		t.Errorf("member reading the owner's calendar: %d; want 403", rec.Code)
	}
	rec = f.as(t, f.secretary, f.h.GetStaffCalendar, "GET", calURL, "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"unavailable":[{`) {
		t.Errorf("secretary's calendar: %d %s; want the block", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest("GET", "/v1/users/me", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, f.secretary))
	me := httptest.NewRecorder()
	f.h.GetMe(me, req)
	if !strings.Contains(me.Body.String(), `"secretary_for":["`+f.owner.ID+`"]`) {
		t.Errorf("GET /v1/users/me = %s; want secretary_for", me.Body.String())
	}
}

// A block takes its range off the booking page and refuses public bookings in it; synced,
// it lands on the host's calendar as a guestless busy event, and deleting it frees the time.
func TestUnavailableBlock_blocksPublicBookingAndSyncs(t *testing.T) {
	f := newStaffFixture(t)
	rec := f.as(t, f.owner, f.h.CreateUnavailableBlock, "POST", "/v1/unavailable-blocks",
		fmt.Sprintf(`{"start_at":%q,"end_at":%q,"title":"Dentist"}`, f.at("10:00"), f.at("11:00")), "")
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"synced":true`) {
		t.Fatalf("create block: %d %s; want a synced block", rec.Code, rec.Body.String())
	}
	blockID := idOf(t, rec)
	ev := f.cal.events()
	if len(ev) != 1 || ev[0].Summary != "Dentist" || ev[0].OrganizerEmail != "" || len(ev[0].Guests) != 0 {
		t.Fatalf("calendar events = %+v; want one guestless 'Dentist' event", ev)
	}

	if code := f.publicBook(t, f.at("10:30")); code != http.StatusConflict {
		t.Errorf("public booking inside the block: %d; want 409", code)
	}
	res, err := f.h.computeSlots(context.Background(), "intro", "UTC", f.day.Format("2006-01-02"), f.day.Format("2006-01-02"), slotsWanted{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Slots) == 0 {
		t.Fatal("no slots at all that day; the check below would pass vacuously")
	}
	for _, s := range res.Slots {
		st, err := time.Parse(time.RFC3339, s.Start)
		if err != nil {
			t.Fatal(err)
		}
		if st.Before(f.day.Add(11*time.Hour)) && st.Add(30*time.Minute).After(f.day.Add(10*time.Hour)) {
			t.Errorf("slot %s offered inside the block", s.Start)
		}
	}

	if rec := f.as(t, f.member, f.h.DeleteUnavailableBlock, "DELETE", "/v1/unavailable-blocks/"+blockID, "", blockID); rec.Code != http.StatusNotFound {
		t.Errorf("member deleting the owner's block: %d; want 404", rec.Code)
	}
	if rec := f.as(t, f.owner, f.h.DeleteUnavailableBlock, "DELETE", "/v1/unavailable-blocks/"+blockID, "", blockID); rec.Code != http.StatusNoContent {
		t.Fatalf("delete block: %d %s", rec.Code, rec.Body.String())
	}
	if code := f.publicBook(t, f.at("10:30")); code != http.StatusCreated {
		t.Errorf("public booking after the block was deleted: %d; want 201", code)
	}
	f.waitFor(t, inviteBookerEmail, 1)
}

// Staff booking over a conflict is a warning: 409 with the reasons, then force books it.
// The past and an identical start stay refused.
func TestStaffBooking_warnsThenForces(t *testing.T) {
	f := newStaffFixture(t)
	if code := f.publicBook(t, f.at("10:00")); code != http.StatusCreated {
		t.Fatalf("public booking: %d", code)
	}
	f.waitFor(t, inviteBookerEmail, 1)
	f.as(t, f.owner, f.h.SetSecretaryHosts, "PUT", "/v1/users/sec-1/secretary-hosts", `{"host_ids":["`+f.owner.ID+`"]}`, f.secretary.ID)

	body := func(start, end string, force bool) string {
		return fmt.Sprintf(`{"host_id":%q,"event_type_id":%q,"start_at":%q,"end_at":%q,"name":"Walk In","email":"walk@in.example","timezone":"Asia/Tehran","guests":["pal@in.example"],"force":%v}`,
			f.owner.ID, f.etID, start, end, force)
	}
	if rec := f.as(t, f.member, f.h.CreateStaffBooking, "POST", "/v1/staff/bookings", body(f.at("14:00"), "", false), ""); rec.Code != http.StatusForbidden {
		t.Errorf("member booking for the owner: %d; want 403", rec.Code)
	}
	rec := f.as(t, f.secretary, f.h.CreateStaffBooking, "POST", "/v1/staff/bookings", body(f.at("10:15"), f.at("11:15"), false), "")
	if rec.Code != http.StatusConflict || strings.Join(conflictKinds(t, rec), ",") != "booking" {
		t.Fatalf("overlapping staff booking: %d %s; want a booking conflict", rec.Code, rec.Body.String())
	}
	rec = f.as(t, f.secretary, f.h.CreateStaffBooking, "POST", "/v1/staff/bookings", body(f.at("10:15"), f.at("11:15"), true), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("forced staff booking: %d %s; want 201", rec.Code, rec.Body.String())
	}
	id := idOf(t, rec)
	var start, end string
	var guests int
	f.h.db.QueryRow(`SELECT start_at, end_at FROM bookings WHERE id = ?`, id).Scan(&start, &end)
	f.h.db.QueryRow(`SELECT COUNT(*) FROM booking_attendees WHERE booking_id = ? AND is_organizer = 0`, id).Scan(&guests)
	if start != f.at("10:15") || end != f.at("11:15") || guests != 1 {
		t.Errorf("stored %s–%s with %d guests; want the dragged 60-minute range and one guest", start, end, guests)
	}
	f.waitFor(t, "walk@in.example", 1)

	rec = f.as(t, f.secretary, f.h.CreateStaffBooking, "POST", "/v1/staff/bookings", body(f.at("10:00"), "", true), "")
	if rec.Code != http.StatusConflict || strings.Join(conflictKinds(t, rec), ",") != "same_start" {
		t.Errorf("forced booking at an identical start: %d %s; want same_start", rec.Code, rec.Body.String())
	}
	past := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute).Format(time.RFC3339)
	rec = f.as(t, f.secretary, f.h.CreateStaffBooking, "POST", "/v1/staff/bookings", body(past, "", true), "")
	if rec.Code != http.StatusConflict || strings.Join(conflictKinds(t, rec), ",") != "past" {
		t.Errorf("forced booking in the past: %d %s; want past", rec.Code, rec.Body.String())
	}

	// The calendar shows both bookings to the secretary as manageable.
	rec = f.as(t, f.secretary, f.h.GetStaffCalendar, "GET",
		"/v1/staff/calendar?from="+f.at("00:00")+"&to="+f.day.Add(24*time.Hour).Format(time.RFC3339)+"&external=1", "", "")
	var cal struct {
		Bookings []staffCalendarBooking `json:"bookings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cal); err != nil || len(cal.Bookings) != 2 || !cal.Bookings[1].CanManage ||
		cal.Bookings[1].AttendeeEmail != "walk@in.example" || len(cal.Bookings[1].Guests) != 1 {
		t.Errorf("calendar = %s; want both bookings, the staff one manageable with its guest", rec.Body.String())
	}
}

// Dragging an appointment moves it (keeping its length unless resized), warns on a
// conflict, and a secretary can cancel it; a member who does not manage the host cannot.
func TestStaffBooking_moveAndCancel(t *testing.T) {
	f := newStaffFixture(t)
	if code := f.publicBook(t, f.at("10:00")); code != http.StatusCreated {
		t.Fatalf("public booking: %d", code)
	}
	f.waitFor(t, inviteBookerEmail, 1)
	var id string
	f.h.db.QueryRow(`SELECT id FROM bookings`).Scan(&id)
	f.as(t, f.owner, f.h.SetSecretaryHosts, "PUT", "/v1/users/sec-1/secretary-hosts", `{"host_ids":["`+f.owner.ID+`"]}`, f.secretary.ID)

	blk := f.as(t, f.owner, f.h.CreateUnavailableBlock, "POST", "/v1/unavailable-blocks",
		fmt.Sprintf(`{"start_at":%q,"end_at":%q,"sync_to_calendar":false}`, f.at("13:00"), f.at("14:00")), "")
	if blk.Code != http.StatusCreated {
		t.Fatalf("block: %d %s", blk.Code, blk.Body.String())
	}

	move := func(user AuthUser, body string) *httptest.ResponseRecorder {
		return f.as(t, user, f.h.MoveStaffBooking, "PATCH", "/v1/staff/bookings/"+id, body, id)
	}
	if rec := move(f.member, `{"start_at":"`+f.at("15:00")+`"}`); rec.Code != http.StatusNotFound {
		t.Errorf("member moving: %d; want 404", rec.Code)
	}
	rec := move(f.secretary, `{"start_at":"`+f.at("13:30")+`"}`)
	if rec.Code != http.StatusConflict || strings.Join(conflictKinds(t, rec), ",") != "unavailable" {
		t.Fatalf("move into a block: %d %s; want an unavailable conflict", rec.Code, rec.Body.String())
	}
	if rec := move(f.secretary, `{"start_at":"`+f.at("15:00")+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("move: %d %s", rec.Code, rec.Body.String())
	}
	if rec := move(f.secretary, `{"start_at":"`+f.at("15:00")+`","end_at":"`+f.at("16:00")+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("resize: %d %s", rec.Code, rec.Body.String())
	}
	var start, end string
	f.h.db.QueryRow(`SELECT start_at, end_at FROM bookings WHERE id = ?`, id).Scan(&start, &end)
	if start != f.at("15:00") || end != f.at("16:00") {
		t.Errorf("booking at %s–%s; want 15:00–16:00", start, end)
	}

	if rec := f.as(t, f.member, f.h.CancelStaffBooking, "POST", "/v1/staff/bookings/"+id+"/cancel", `{}`, id); rec.Code != http.StatusNotFound {
		t.Errorf("member cancelling: %d; want 404", rec.Code)
	}
	if rec := f.as(t, f.secretary, f.h.CancelStaffBooking, "POST", "/v1/staff/bookings/"+id+"/cancel", `{}`, id); rec.Code != http.StatusOK {
		t.Fatalf("secretary cancelling: %d %s", rec.Code, rec.Body.String())
	}
	var status string
	f.h.db.QueryRow(`SELECT status FROM bookings WHERE id = ?`, id).Scan(&status)
	if status != "cancelled" {
		t.Errorf("status = %s; want cancelled", status)
	}
	f.waitFor(t, inviteBookerEmail, 3) // confirmation, reschedule notices and the cancellation land in the background
}

// The Schedule's one attendee list: guests can carry names, the calendar returns them with
// the booker's zone, and the primary host's secretary can add another attendee later.
func TestStaffBooking_namedGuestsZoneAndSecretaryAddsGuest(t *testing.T) {
	f := newStaffFixture(t)
	f.as(t, f.owner, f.h.SetSecretaryHosts, "PUT", "/v1/users/sec-1/secretary-hosts", `{"host_ids":["`+f.owner.ID+`"]}`, f.secretary.ID)

	body := fmt.Sprintf(`{"host_id":%q,"event_type_id":%q,"start_at":%q,"name":"Walk In","email":"walk@in.example",
		"timezone":"America/New_York","guests":[{"name":"Pat Pal","email":"pal@in.example"},"plain@in.example"]}`,
		f.owner.ID, f.etID, f.at("14:00"))
	rec := f.as(t, f.secretary, f.h.CreateStaffBooking, "POST", "/v1/staff/bookings", body, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("staff booking: %d %s", rec.Code, rec.Body.String())
	}
	id := idOf(t, rec)
	f.waitFor(t, "walk@in.example", 1)

	calendar := func() staffCalendarBooking {
		t.Helper()
		rec := f.as(t, f.secretary, f.h.GetStaffCalendar, "GET",
			"/v1/staff/calendar?from="+f.at("00:00")+"&to="+f.day.Add(24*time.Hour).Format(time.RFC3339), "", "")
		var cal struct {
			Bookings []staffCalendarBooking `json:"bookings"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &cal); err != nil || len(cal.Bookings) != 1 {
			t.Fatalf("calendar = %d %s", rec.Code, rec.Body.String())
		}
		return cal.Bookings[0]
	}
	b := calendar()
	if b.AttendeeTimezone != "America/New_York" {
		t.Errorf("attendee_timezone = %q; want the zone the appointment was booked in", b.AttendeeTimezone)
	}
	want := []guestJSON{{Name: "Pat Pal", Email: "pal@in.example"}, {Name: "", Email: "plain@in.example"}}
	if fmt.Sprint(b.GuestList) != fmt.Sprint(want) {
		t.Errorf("guest_list = %+v; want %+v", b.GuestList, want)
	}

	if rec := f.as(t, f.member, f.h.AddBookingGuest, "POST", "/v1/bookings/"+id+"/guests", `{"email":"x@in.example"}`, id); rec.Code != http.StatusNotFound {
		t.Errorf("member adding a guest: %d; want 404", rec.Code)
	}
	rec = f.as(t, f.secretary, f.h.AddBookingGuest, "POST", "/v1/bookings/"+id+"/guests", `{"name":"Late Comer","email":"late@in.example"}`, id)
	if rec.Code != http.StatusCreated {
		t.Fatalf("secretary adding a guest: %d %s; want 201", rec.Code, rec.Body.String())
	}
	if got := calendar().GuestList; len(got) != 3 || got[0] != (guestJSON{Name: "Late Comer", Email: "late@in.example"}) {
		t.Errorf("guest_list after add = %+v; want Late Comer added", got)
	}
}
