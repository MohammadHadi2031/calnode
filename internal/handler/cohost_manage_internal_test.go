package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

// A co-host of a collective booking manages it like the primary host: reschedule and
// cancel on the Bookings page, and move it on the Schedule page, which marks it
// can_manage. Both used to answer "booking not found" because only the primary host,
// an admin or the primary host's secretary were let through. Someone with no seat on
// the booking still gets 404.
func TestCoHost_reschedulesMovesAndCancelsASharedBooking(t *testing.T) {
	f := newStaffFixture(t)
	f.makeCollective(t)
	for d := 0; d < 7; d++ {
		if _, err := f.h.db.Exec(`INSERT INTO availability_rules (id, user_id, day_of_week, start_time, end_time) VALUES (?, ?, ?, '00:00', '23:59')`,
			fmt.Sprintf("mem-rule-%d", d), f.member.ID, d); err != nil {
			t.Fatal(err)
		}
	}

	rec := f.as(t, f.owner, f.h.CreateStaffBooking, "POST", "/v1/staff/bookings", fmt.Sprintf(
		`{"host_id":%q,"event_type_id":%q,"start_at":%q,"name":"Walk In","email":"walk@in.example","force":true}`,
		f.owner.ID, f.etID, f.at("10:00")), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("staff booking: %d %s", rec.Code, rec.Body.String())
	}
	id := idOf(t, rec)
	f.waitFor(t, "walk@in.example", 1)
	var seat int
	f.h.db.QueryRow(`SELECT COUNT(*) FROM booking_hosts WHERE booking_id = ? AND user_id = ?`, id, f.member.ID).Scan(&seat)
	if seat != 1 {
		t.Fatalf("the member has no seat on the collective booking")
	}

	// Someone without a seat (a secretary of nobody) still can't touch it.
	outsider := f.secretary
	if rec := f.as(t, outsider, f.h.RescheduleBooking, "PATCH", "/v1/bookings/"+id+"/reschedule", `{"start_at":"`+f.at("14:00")+`"}`, id); rec.Code != http.StatusNotFound {
		t.Errorf("outsider rescheduling: %d; want 404", rec.Code)
	}
	if rec := f.as(t, outsider, f.h.MoveStaffBooking, "PATCH", "/v1/staff/bookings/"+id, `{"start_at":"`+f.at("14:00")+`"}`, id); rec.Code != http.StatusNotFound {
		t.Errorf("outsider moving: %d; want 404", rec.Code)
	}
	if rec := f.as(t, outsider, f.h.CancelBooking, "POST", "/v1/bookings/"+id+"/cancel", `{}`, id); rec.Code != http.StatusNotFound {
		t.Errorf("outsider cancelling: %d; want 404", rec.Code)
	}

	// The co-host's Schedule marks the shared booking as theirs to manage.
	q := url.Values{"from": {f.at("00:00")}, "to": {f.at("23:59")}}
	rec = f.as(t, f.member, f.h.GetStaffCalendar, "GET", "/v1/staff/calendar?"+q.Encode(), "", "")
	var cal struct {
		Bookings []struct {
			ID        string `json:"id"`
			CanManage bool   `json:"can_manage"`
		} `json:"bookings"`
	}
	json.Unmarshal(rec.Body.Bytes(), &cal)
	if rec.Code != http.StatusOK || len(cal.Bookings) != 1 || cal.Bookings[0].ID != id || !cal.Bookings[0].CanManage {
		t.Errorf("co-host's calendar: %d %s; want the booking with can_manage", rec.Code, rec.Body.String())
	}

	if rec := f.as(t, f.member, f.h.RescheduleBooking, "PATCH", "/v1/bookings/"+id+"/reschedule", `{"start_at":"`+f.at("14:00")+`"}`, id); rec.Code != http.StatusOK {
		t.Fatalf("co-host rescheduling: %d %s", rec.Code, rec.Body.String())
	}
	f.waitFor(t, "walk@in.example", 2)

	if rec := f.as(t, f.member, f.h.MoveStaffBooking, "PATCH", "/v1/staff/bookings/"+id, `{"start_at":"`+f.at("15:00")+`"}`, id); rec.Code != http.StatusOK {
		t.Fatalf("co-host moving: %d %s", rec.Code, rec.Body.String())
	}
	f.waitFor(t, "walk@in.example", 3)

	if rec := f.as(t, f.member, f.h.CancelBooking, "POST", "/v1/bookings/"+id+"/cancel", `{}`, id); rec.Code != http.StatusOK {
		t.Fatalf("co-host cancelling: %d %s", rec.Code, rec.Body.String())
	}
	var status string
	f.h.db.QueryRow(`SELECT status FROM bookings WHERE id = ?`, id).Scan(&status)
	if status != "cancelled" {
		t.Errorf("status = %q; want cancelled", status)
	}
	f.waitFor(t, "walk@in.example", 4)
	if rec := f.as(t, f.member, f.h.CancelBooking, "POST", "/v1/bookings/"+id+"/cancel", `{}`, id); rec.Code != http.StatusConflict {
		t.Errorf("cancelling twice: %d; want 409", rec.Code)
	}
}
