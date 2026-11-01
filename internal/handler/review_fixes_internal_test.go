package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// makeCollective turns the fixture's "intro" into a collective with the owner (primary)
// and the member as required hosts. The member has no working hours.
func (f *staffFixture) makeCollective(t *testing.T) {
	t.Helper()
	for _, q := range []string{
		`UPDATE event_types SET routing_mode = 'collective' WHERE id = '` + f.etID + `'`,
		`INSERT OR IGNORE INTO event_type_hosts (id, event_type_id, user_id, role, priority) VALUES ('eth-1', '` + f.etID + `', '` + f.owner.ID + `', 'required', 0)`,
		`UPDATE event_type_hosts SET role = 'required', priority = 0 WHERE event_type_id = '` + f.etID + `' AND user_id = '` + f.owner.ID + `'`,
		`INSERT INTO event_type_hosts (id, event_type_id, user_id, role, priority) VALUES ('eth-2', '` + f.etID + `', '` + f.member.ID + `', 'required', 1)`,
	} {
		if _, err := f.h.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// Force overrides a conflict only for a host the caller manages. A secretary of the
// primary host used to be able to force-book a co-host she has no say over (here: at a
// time outside the co-host's hours); now that conflict comes back blocking, even with
// force. An admin, who manages everyone, still can.
func TestStaffBooking_forceDoesNotOverrideAnUnmanagedCoHost(t *testing.T) {
	f := newStaffFixture(t)
	f.makeCollective(t)
	f.as(t, f.owner, f.h.SetSecretaryHosts, "PUT", "/v1/users/sec-1/secretary-hosts", `{"host_ids":["`+f.owner.ID+`"]}`, f.secretary.ID)

	body := func(force bool, guests string) string {
		return fmt.Sprintf(`{"host_id":%q,"event_type_id":%q,"start_at":%q,"name":"Walk In","email":"walk@in.example","guests":[%s],"force":%v}`,
			f.owner.ID, f.etID, f.at("10:00"), guests, force)
	}
	for _, force := range []bool{false, true} {
		rec := f.as(t, f.secretary, f.h.CreateStaffBooking, "POST", "/v1/staff/bookings", body(force, ""), "")
		var resp struct {
			Conflicts []staffConflict `json:"conflicts"`
		}
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if rec.Code != http.StatusConflict || len(resp.Conflicts) != 1 || resp.Conflicts[0].HostID != f.member.ID || !resp.Conflicts[0].Blocking {
			t.Fatalf("secretary, force=%v: %d %s; want one blocking conflict for the co-host", force, rec.Code, rec.Body.String())
		}
	}

	// The admin may force it, and a host's address in the guest list is dropped (the
	// member is a host of this event type, so they are on the meeting anyway).
	rec := f.as(t, f.owner, f.h.CreateStaffBooking, "POST", "/v1/staff/bookings",
		body(true, `"`+f.member.Email+`","pal@in.example"`), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin, force: %d %s; want 201", rec.Code, rec.Body.String())
	}
	id := idOf(t, rec)
	var guests []string
	rows, _ := f.h.db.Query(`SELECT email FROM booking_attendees WHERE booking_id = ? AND is_organizer = 0`, id)
	for rows.Next() {
		var e string
		rows.Scan(&e)
		guests = append(guests, e)
	}
	rows.Close()
	if strings.Join(guests, ",") != "pal@in.example" {
		t.Errorf("guests = %v; want only pal@in.example (the co-host's address dropped)", guests)
	}
	f.waitFor(t, "walk@in.example", 1)

	// Moving it as the secretary, with force, is refused for the same reason.
	rec = f.as(t, f.secretary, f.h.MoveStaffBooking, "PATCH", "/v1/staff/bookings/"+id,
		`{"start_at":"`+f.at("11:00")+`","force":true}`, id)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"blocking":true`) {
		t.Errorf("secretary forced move: %d %s; want a blocking conflict", rec.Code, rec.Body.String())
	}
}

// The public booking page drops a guest whose address is the host's.
func TestPublicBooking_dropsTheHostAsAGuest(t *testing.T) {
	f := newStaffFixture(t)
	req := fmt.Sprintf(`{"event_type_slug":"intro","start_at":%q,"name":"Guest","email":%q,"timezone":"UTC","guests":[%q,"friend@booker.example"]}`,
		f.at("10:00"), inviteBookerEmail, strings.ToUpper(inviteHostEmail))
	rec := f.as(t, AuthUser{}, f.h.CreateBooking, "POST", "/v1/bookings", req, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("booking: %d %s", rec.Code, rec.Body.String())
	}
	var n int
	f.h.db.QueryRow(`SELECT COUNT(*) FROM booking_attendees WHERE is_organizer = 0 AND lower(email) = lower(?)`, inviteHostEmail).Scan(&n)
	if n != 0 {
		t.Errorf("the host was stored as a guest")
	}
	f.h.db.QueryRow(`SELECT COUNT(*) FROM booking_attendees WHERE is_organizer = 0`).Scan(&n)
	if n != 1 {
		t.Errorf("guests = %d; want 1 (the friend)", n)
	}
	f.waitFor(t, inviteBookerEmail, 1)
}

// A secretary sees the bookings of the hosts they manage on the Bookings page
// (?scope=managed), and can open, reschedule and cancel them there. Rescheduling keeps
// the appointment's own length instead of resetting it to the event type's.
func TestSecretary_bookingsPageListsReschedulesAndCancels(t *testing.T) {
	f := newStaffFixture(t)
	f.as(t, f.owner, f.h.SetSecretaryHosts, "PUT", "/v1/users/sec-1/secretary-hosts", `{"host_ids":["`+f.owner.ID+`"]}`, f.secretary.ID)
	// A 60-minute appointment on a 30-minute event type.
	rec := f.as(t, f.owner, f.h.CreateStaffBooking, "POST", "/v1/staff/bookings", fmt.Sprintf(
		`{"host_id":%q,"event_type_id":%q,"start_at":%q,"end_at":%q,"name":"Walk In","email":"walk@in.example"}`,
		f.owner.ID, f.etID, f.at("10:00"), f.at("11:00")), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("staff booking: %d %s", rec.Code, rec.Body.String())
	}
	id := idOf(t, rec)
	f.waitFor(t, "walk@in.example", 1)

	list := func(user AuthUser, q string) []string {
		rec := f.as(t, user, f.h.ListBookings, "GET", "/v1/bookings"+q, "", "")
		var resp struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		json.Unmarshal(rec.Body.Bytes(), &resp)
		var ids []string
		for _, it := range resp.Items {
			ids = append(ids, it.ID)
		}
		return ids
	}
	if got := list(f.secretary, ""); len(got) != 0 {
		t.Errorf("secretary's own bookings = %v; want none", got)
	}
	if got := list(f.secretary, "?scope=managed"); len(got) != 1 || got[0] != id {
		t.Errorf("secretary's managed bookings = %v; want [%s]", got, id)
	}
	if got := list(f.member, "?scope=managed"); len(got) != 0 {
		t.Errorf("a non-secretary's managed bookings = %v; want none", got)
	}

	if rec := f.as(t, f.secretary, f.h.GetBookingAnswers, "GET", "/v1/bookings/"+id+"/answers", "", id); rec.Code == http.StatusNotFound {
		t.Errorf("secretary reading answers: 404")
	}
	if rec := f.as(t, f.member, f.h.RescheduleBooking, "PATCH", "/v1/bookings/"+id+"/reschedule", `{"start_at":"`+f.at("14:00")+`"}`, id); rec.Code != http.StatusNotFound {
		t.Errorf("member rescheduling: %d; want 404", rec.Code)
	}
	if rec := f.as(t, f.secretary, f.h.RescheduleBooking, "PATCH", "/v1/bookings/"+id+"/reschedule", `{"start_at":"`+f.at("14:00")+`"}`, id); rec.Code != http.StatusOK {
		t.Fatalf("secretary rescheduling: %d %s", rec.Code, rec.Body.String())
	}
	var start, end string
	f.h.db.QueryRow(`SELECT start_at, end_at FROM bookings WHERE id = ?`, id).Scan(&start, &end)
	if start != f.at("14:00") || end != f.at("15:00") {
		t.Errorf("rescheduled to %s–%s; want 14:00–15:00 (its 60 minutes kept)", start, end)
	}

	if rec := f.as(t, f.member, f.h.CancelBooking, "POST", "/v1/bookings/"+id+"/cancel", `{}`, id); rec.Code != http.StatusNotFound {
		t.Errorf("member cancelling: %d; want 404", rec.Code)
	}
	if rec := f.as(t, f.secretary, f.h.CancelBooking, "POST", "/v1/bookings/"+id+"/cancel", `{}`, id); rec.Code != http.StatusOK {
		t.Fatalf("secretary cancelling: %d %s", rec.Code, rec.Body.String())
	}
	f.waitFor(t, "walk@in.example", 3)
}

// An appointment that has already ended can't be dragged to a new time.
func TestStaffBooking_cannotMoveAnEndedAppointment(t *testing.T) {
	f := newStaffFixture(t)
	past := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)
	if _, err := f.h.db.Exec(`INSERT INTO bookings (id, event_type_id, host_id, start_at, end_at, status) VALUES ('old-1', ?, ?, ?, ?, 'confirmed')`,
		f.etID, f.owner.ID, past.Format(time.RFC3339), past.Add(30*time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	rec := f.as(t, f.owner, f.h.MoveStaffBooking, "PATCH", "/v1/staff/bookings/old-1", `{"start_at":"`+f.at("10:00")+`"}`, "old-1")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already ended") {
		t.Errorf("moving an ended appointment: %d %s; want 409 already ended", rec.Code, rec.Body.String())
	}
}
