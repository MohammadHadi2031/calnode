package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/calnode/calnode/internal/booking"
	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/i18n"
	"github.com/calnode/calnode/internal/slots"
)

// Fork (wisegrid): the staff Schedule page's API - a calendar of the hosts the caller
// manages, and appointments created, moved and cancelled from it by the host, an admin
// or the host's secretary. Conflicts are warnings the caller confirms with force.
// See docs/fork-staff-calendar.md.

const maxCalendarRange = 62 * 24 * time.Hour

const (
	bookingSeatsQuery pairQuery = `SELECT booking_id, user_id FROM booking_hosts
		WHERE booking_id IN (%s) ORDER BY is_primary DESC, user_id`
	bookingGuestEmailsQuery pairQuery = `SELECT booking_id, email FROM booking_attendees
		WHERE booking_id IN (%s) AND is_organizer = 0 ORDER BY email`
	// Name and email joined by a unit separator, which neither can contain.
	bookingGuestDetailsQuery pairQuery = `SELECT booking_id, COALESCE(name, '') || char(31) || email FROM booking_attendees
		WHERE booking_id IN (%s) AND is_organizer = 0 ORDER BY email`
)

// staffConflict is one reason a staff action's time does not work for a host.
type staffConflict struct {
	HostID   string `json:"host_id"`
	HostName string `json:"host_name"`
	// Kind: past | same_start (never overridable) | outside_hours | booking | unavailable |
	// calendar | calendar_unchecked.
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

func (h *Handler) writeConflicts(w http.ResponseWriter, conflicts []staffConflict) {
	h.writeJSON(w, http.StatusConflict, map[string]any{"error": "conflicts", "conflicts": conflicts})
}

func (h *Handler) userName(ctx context.Context, userID string) string {
	var name, email string
	if err := h.db.QueryRowContext(ctx, `SELECT name, email FROM users WHERE id = ?`, userID).Scan(&name, &email); err != nil {
		return ""
	}
	if strings.TrimSpace(name) == "" {
		return email
	}
	return name
}

// staffConflicts checks [start, end) for every host in hostIDs: inside their working
// hours for eventTypeID, clear of unavailable blocks and other bookings
// (excludeBookingID: the one being moved), and free on their external calendars.
func (h *Handler) staffConflicts(ctx context.Context, eventTypeID string, hostIDs []string, start, end time.Time, excludeBookingID string) ([]staffConflict, error) {
	var out []staffConflict
	names := map[string]string{}
	for _, id := range hostIDs {
		names[id] = h.userName(ctx, id)
		within, err := h.hostWithinHours(ctx, id, eventTypeID, start, end)
		if err != nil {
			return nil, err
		}
		if !within {
			out = append(out, staffConflict{HostID: id, HostName: names[id], Kind: "outside_hours", Detail: "outside working hours"})
		}
		blocked, err := h.hostBlocked(ctx, id, start, end)
		if err != nil {
			return nil, err
		}
		if blocked {
			out = append(out, staffConflict{HostID: id, HostName: names[id], Kind: "unavailable", Detail: "during an unavailable block"})
		}
		overlaps, err := h.bookingOverlapConflicts(ctx, id, names[id], start, end, excludeBookingID)
		if err != nil {
			return nil, err
		}
		out = append(out, overlaps...)
	}

	gc := h.getCal()
	if gc == nil || !gc.Any() {
		return out, nil
	}
	cctx, cancel := context.WithTimeout(ctx, calendarCheckTimeout)
	defer cancel()
	window := slots.Interval{Start: start, End: end}
	type verdict struct {
		free bool
		err  error
	}
	verdicts := make([]verdict, len(hostIDs))
	var wg sync.WaitGroup
	for i, id := range hostIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			free, err := h.calendarFreeAt(cctx, gc, id, window)
			verdicts[i] = verdict{free, err}
		}()
	}
	wg.Wait()
	for i, id := range hostIDs {
		switch v := verdicts[i]; {
		case v.err != nil:
			h.logger.WarnContext(ctx, "staff calendar: free/busy check", "host", id, "error", v.err)
			out = append(out, staffConflict{HostID: id, HostName: names[id], Kind: "calendar_unchecked", Detail: "their calendar could not be checked"})
		case !v.free:
			out = append(out, staffConflict{HostID: id, HostName: names[id], Kind: "calendar", Detail: "busy on their calendar"})
		}
	}
	return out, nil
}

// pastConflict is the one check force cannot skip.
func pastConflict(start time.Time) []staffConflict {
	if start.Before(time.Now().Add(-time.Minute)) {
		return []staffConflict{{Kind: "past", Detail: "the time is in the past"}}
	}
	return nil
}

func sameStartConflict(hostID, hostName string) []staffConflict {
	return []staffConflict{{HostID: hostID, HostName: hostName, Kind: "same_start",
		Detail: "another appointment for this host starts at exactly this time"}}
}

// --- GET /v1/staff/hosts --------------------------------------------------------------

type staffEventType struct {
	ID                    string `json:"id"`
	Slug                  string `json:"slug"`
	Name                  string `json:"name"`
	DurationMinutes       int    `json:"duration_minutes"`
	LocationType          string `json:"location_type"`
	AllowConferenceChoice bool   `json:"allow_conference_choice"`
}

// ListStaffHosts handles GET /v1/staff/hosts.
func (h *Handler) ListStaffHosts(w http.ResponseWriter, r *http.Request) {
	actor, _ := userFromContext(r.Context())
	hosts, err := h.managedHosts(r.Context(), actor)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "staff hosts", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	type hostOut struct {
		managedHost
		HasCalendar bool             `json:"has_calendar"`
		EventTypes  []staffEventType `json:"event_types"`
	}
	out := make([]hostOut, 0, len(hosts))
	gc := h.getCal()
	for _, m := range hosts {
		ho := hostOut{managedHost: m, EventTypes: []staffEventType{}}
		rows, err := h.db.QueryContext(r.Context(), `
			SELECT et.id, et.slug, et.name, et.duration_minutes, et.location_type, et.allow_conference_choice
			FROM event_types et JOIN event_type_hosts eth ON eth.event_type_id = et.id
			WHERE eth.user_id = ? AND et.is_active = 1 AND et.archived_at IS NULL
			ORDER BY et.display_order, et.name COLLATE NOCASE`, m.ID)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "staff hosts: event types", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		for rows.Next() {
			var e staffEventType
			if err := rows.Scan(&e.ID, &e.Slug, &e.Name, &e.DurationMinutes, &e.LocationType, &e.AllowConferenceChoice); err != nil {
				rows.Close() // #nosec G104 -- already returning the scan error
				h.logger.ErrorContext(r.Context(), "staff hosts: scan event type", "error", err)
				h.writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			ho.EventTypes = append(ho.EventTypes, e)
		}
		rows.Close() // #nosec G104 -- fully consumed
		if gc != nil {
			ho.HasCalendar, _ = gc.HasDestination(r.Context(), m.ID)
		}
		out = append(out, ho)
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"hosts": out})
}

// --- GET /v1/staff/calendar -----------------------------------------------------------

type staffCalendarBooking struct {
	ID            string    `json:"id"`
	EventTypeID   string    `json:"event_type_id"`
	EventTypeName string    `json:"event_type_name"`
	PrimaryHostID string    `json:"primary_host_id"`
	HostIDs       []string  `json:"host_ids"`
	StartAt       time.Time `json:"start_at"`
	EndAt         time.Time `json:"end_at"`
	Status        string    `json:"status"`
	LocationType  string    `json:"location_type"`
	LocationValue string    `json:"location_value"`
	AttendeeName  string    `json:"attendee_name"`
	AttendeeEmail string    `json:"attendee_email"`
	// AttendeeTimezone is the booker's zone: the zone the booking's emails to the booker
	// and guests are written in.
	AttendeeTimezone string      `json:"attendee_timezone"`
	Guests           []string    `json:"guests"`
	GuestList        []guestJSON `json:"guest_list"`
	CanManage        bool        `json:"can_manage"`
}

type staffBusy struct {
	HostID  string    `json:"host_id"`
	StartAt time.Time `json:"start_at"`
	EndAt   time.Time `json:"end_at"`
}

// GetStaffCalendar handles GET /v1/staff/calendar?from&to&host_ids&external.
func (h *Handler) GetStaffCalendar(w http.ResponseWriter, r *http.Request) {
	actor, _ := userFromContext(r.Context())
	ctx := r.Context()
	q := r.URL.Query()
	from, err1 := time.Parse(time.RFC3339, q.Get("from"))
	to, err2 := time.Parse(time.RFC3339, q.Get("to"))
	if err1 != nil || err2 != nil || !to.After(from) {
		h.writeError(w, http.StatusBadRequest, "from and to must be RFC3339 with to after from")
		return
	}
	if to.Sub(from) > maxCalendarRange {
		h.writeError(w, http.StatusBadRequest, "the range can be at most 62 days")
		return
	}
	from, to = from.UTC(), to.UTC()

	managed, err := h.managedHosts(ctx, actor)
	if err != nil {
		h.logger.ErrorContext(ctx, "staff calendar: hosts", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	manageable := map[string]bool{}
	var hostIDs []string
	for _, m := range managed {
		manageable[m.ID] = true
		hostIDs = append(hostIDs, m.ID)
	}
	if raw := strings.TrimSpace(q.Get("host_ids")); raw != "" {
		hostIDs = nil
		for _, id := range strings.Split(raw, ",") {
			id = strings.TrimSpace(id)
			if id == "" || slices.Contains(hostIDs, id) {
				continue
			}
			if !manageable[id] {
				h.writeError(w, http.StatusForbidden, "you cannot view this host's calendar")
				return
			}
			hostIDs = append(hostIDs, id)
		}
	}

	bookings, err := h.staffCalendarBookings(ctx, hostIDs, from, to, manageable)
	if err != nil {
		h.logger.ErrorContext(ctx, "staff calendar: bookings", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	blocks := []unavailableBlock{}
	for _, id := range hostIDs {
		bs, err := h.blocksBetween(ctx, id, from, to)
		if err != nil {
			h.logger.ErrorContext(ctx, "staff calendar: blocks", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		blocks = append(blocks, bs...)
	}

	busy := []staffBusy{}
	type busyErr struct {
		HostID string `json:"host_id"`
		Error  string `json:"error"`
	}
	busyErrors := []busyErr{}
	if gc := h.getCal(); q.Get("external") == "1" && gc != nil && gc.Any() {
		own := make([][]slots.Interval, len(hostIDs))
		for i, id := range hostIDs { // DB reads before the provider fan-out (single connection)
			if own[i], err = h.ownCalendarEvents(ctx, id, from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano)); err != nil {
				h.logger.ErrorContext(ctx, "staff calendar: own events", "error", err)
				h.writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
		}
		cctx, cancel := context.WithTimeout(ctx, calendarCheckTimeout)
		defer cancel()
		results := make([][]slots.Interval, len(hostIDs))
		errs := make([]error, len(hostIDs))
		var wg sync.WaitGroup
		for i, id := range hostIDs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i], errs[i] = gc.FreeBusy(cctx, id, from, to)
			}()
		}
		wg.Wait()
		for i, id := range hostIDs {
			if errs[i] != nil {
				h.logger.WarnContext(ctx, "staff calendar: free/busy", "host", id, "error", errs[i])
				busyErrors = append(busyErrors, busyErr{HostID: id, Error: calendar.UserMessage(errs[i])})
				continue
			}
			for _, iv := range slots.SubtractIntervals(results[i], own[i]) {
				busy = append(busy, staffBusy{HostID: id, StartAt: iv.Start, EndAt: iv.End})
			}
		}
	}

	h.writeJSON(w, http.StatusOK, map[string]any{
		"bookings": bookings, "unavailable": blocks, "busy": busy, "busy_errors": busyErrors,
	})
}

func (h *Handler) staffCalendarBookings(ctx context.Context, hostIDs []string, from, to time.Time, manageable map[string]bool) ([]staffCalendarBooking, error) {
	out := []staffCalendarBooking{}
	if len(hostIDs) == 0 {
		return out, nil
	}
	args := []any{}
	for _, id := range hostIDs {
		args = append(args, id)
	}
	args = append(args, to.Format(time.RFC3339Nano), from.Format(time.RFC3339Nano))
	rows, err := h.db.QueryContext(ctx, `
		SELECT DISTINCT b.id, b.event_type_id, COALESCE(et.name, ''), b.host_id, b.start_at, b.end_at, b.status,
		       COALESCE(b.location_type, ''), COALESCE(b.location_value, ''),
		       COALESCE(a.name, ''), COALESCE(a.email, ''), COALESCE(a.iana_timezone, '')
		FROM bookings b
		JOIN booking_hosts bh ON bh.booking_id = b.id
		LEFT JOIN event_types et ON et.id = b.event_type_id
		LEFT JOIN booking_attendees a ON a.booking_id = b.id AND a.is_organizer = 1
		WHERE bh.user_id IN (`+placeholders(len(hostIDs))+`) AND b.status != 'cancelled'
		  AND b.start_at < ? AND b.end_at > ?
		ORDER BY b.start_at LIMIT 2000`, args...)
	if err != nil {
		return nil, err
	}
	index := map[string]int{}
	for rows.Next() {
		var c staffCalendarBooking
		var s, e string
		if err := rows.Scan(&c.ID, &c.EventTypeID, &c.EventTypeName, &c.PrimaryHostID, &s, &e, &c.Status,
			&c.LocationType, &c.LocationValue, &c.AttendeeName, &c.AttendeeEmail, &c.AttendeeTimezone); err != nil {
			rows.Close() // #nosec G104 -- already returning the scan error
			return nil, err
		}
		c.StartAt, _ = time.Parse(time.RFC3339Nano, s)
		c.EndAt, _ = time.Parse(time.RFC3339Nano, e)
		c.CanManage = manageable[c.PrimaryHostID]
		c.HostIDs, c.Guests, c.GuestList = []string{}, []string{}, []guestJSON{}
		index[c.ID] = len(out)
		out = append(out, c)
	}
	rows.Close() // #nosec G104 -- fully consumed
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	ids := make([]any, 0, len(out))
	for _, c := range out {
		ids = append(ids, c.ID)
	}
	if err := h.scanPairs(ctx, bookingSeatsQuery, ids,
		func(id, v string) { out[index[id]].HostIDs = append(out[index[id]].HostIDs, v) }); err != nil {
		return nil, err
	}
	if err := h.scanPairs(ctx, bookingGuestEmailsQuery, ids,
		func(id, v string) { out[index[id]].Guests = append(out[index[id]].Guests, v) }); err != nil {
		return nil, err
	}
	if err := h.scanPairs(ctx, bookingGuestDetailsQuery, ids, func(id, v string) {
		name, email, _ := strings.Cut(v, "")
		out[index[id]].GuestList = append(out[index[id]].GuestList, guestJSON{Name: name, Email: email})
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// --- POST /v1/staff/bookings ----------------------------------------------------------

// loadStaffEventType is loadBookableEventType by id without the is_public gate: staff
// book unlisted event types too. Inactive and archived ones stay unbookable.
func (h *Handler) loadStaffEventType(ctx context.Context, id string) (*bookableEventType, string, error) {
	var slug string
	if err := h.db.QueryRowContext(ctx,
		`SELECT slug FROM event_types WHERE id = ? AND is_active = 1 AND archived_at IS NULL`, id).Scan(&slug); err != nil {
		return nil, "", errEventTypeNotFound
	}
	var et bookableEventType
	err := h.db.QueryRowContext(ctx, `
		SELECT id, user_id, name, duration_minutes, location_type, location_value, routing_mode, rr_strategy,
		       invite_delivery, max_guests, allow_conference_choice
		FROM event_types WHERE id = ?`, id).
		Scan(&et.ID, &et.UserID, &et.Name, &et.DurationMinutes, &et.LocationType, &et.LocationValue,
			&et.RoutingMode, &et.RRStrategy, &et.InviteDelivery, &et.MaxGuests, &et.AllowConferenceChoice)
	if err != nil {
		return nil, "", errEventTypeNotFound
	}
	return &et, slug, nil
}

// staffGuest is one guest in a staff booking request: an email string, or {name, email}.
type staffGuest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (g *staffGuest) UnmarshalJSON(b []byte) error {
	var email string
	if err := json.Unmarshal(b, &email); err == nil {
		*g = staffGuest{Email: email}
		return nil
	}
	type plain staffGuest
	return json.Unmarshal(b, (*plain)(g))
}

// nameGuests copies the names given in the request onto the normalized guests, matching
// by email. Names are capped at 200 characters, the same as a guest a host adds later.
func nameGuests(guests []booking.Attendee, given []staffGuest) {
	for i := range guests {
		for _, g := range given {
			if strings.EqualFold(strings.TrimSpace(g.Email), guests[i].Email) {
				name := []rune(strings.TrimSpace(g.Name))
				if len(name) > 200 {
					name = name[:200]
				}
				guests[i].Name = string(name)
				break
			}
		}
	}
}

// CreateStaffBooking handles POST /v1/staff/bookings.
func (h *Handler) CreateStaffBooking(w http.ResponseWriter, r *http.Request) {
	actor, _ := userFromContext(r.Context())
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	var req struct {
		HostID      string       `json:"host_id"`
		EventTypeID string       `json:"event_type_id"`
		StartAt     string       `json:"start_at"`
		EndAt       string       `json:"end_at"`
		Name        string       `json:"name"`
		Email       string       `json:"email"`
		Timezone    string       `json:"timezone"`
		Language    string       `json:"language"`
		Guests      []staffGuest `json:"guests"`
		Conference  string       `json:"conference"`
		Force       bool         `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.HostID == "" || req.EventTypeID == "" || req.StartAt == "" || req.Name == "" || req.Email == "" {
		h.writeError(w, http.StatusBadRequest, "host_id, event_type_id, start_at, name and email are required")
		return
	}
	email, err := normalizeBookerEmail(req.Email)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if ok, err := h.canManageHost(ctx, actor, req.HostID); err != nil {
		h.logger.ErrorContext(ctx, "staff booking: access", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	} else if !ok {
		h.writeError(w, http.StatusForbidden, "you cannot manage this host's calendar")
		return
	}
	et, slug, err := h.loadStaffEventType(ctx, req.EventTypeID)
	if err != nil {
		h.writeError(w, http.StatusNotFound, "event type not found")
		return
	}
	start, err := time.Parse(time.RFC3339, req.StartAt)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "start_at must be RFC3339 (e.g. 2026-06-15T09:00:00Z)")
		return
	}
	start = start.UTC()
	end := start.Add(time.Duration(et.DurationMinutes) * time.Minute)
	if req.EndAt != "" {
		if end, err = time.Parse(time.RFC3339, req.EndAt); err != nil {
			h.writeError(w, http.StatusBadRequest, "end_at must be RFC3339")
			return
		}
		end = end.UTC()
	}
	if !end.After(start) || end.Sub(start) > 24*time.Hour {
		h.writeError(w, http.StatusBadRequest, "end_at must be after start_at and at most 24 hours later")
		return
	}
	if req.Timezone == "" {
		req.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(req.Timezone); err != nil {
		h.writeError(w, http.StatusBadRequest, "timezone must be an IANA zone (e.g. Europe/Berlin)")
		return
	}
	loc := i18n.Resolve("", req.Language)
	guestEmails := make([]string, len(req.Guests))
	for i, g := range req.Guests {
		guestEmails[i] = g.Email
	}
	guests, err := normalizeGuests(guestEmails, email, maxGuestsCeiling, loc)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	nameGuests(guests, req.Guests)
	locValue := ""
	if et.LocationValue != nil {
		locValue = *et.LocationValue
	}
	if !applyConferenceChoice(et, strings.TrimSpace(req.Conference), &locValue) {
		h.writeError(w, http.StatusBadRequest, "conference must be google_meet or teams, and offered by this event type")
		return
	}

	// The event type's routing, with the chosen host in the seat they were picked for:
	// round-robin assigns them instead of rotating; a collective puts them first, so they
	// are the primary host.
	hosts, err := h.resolveEventTypeHosts(ctx, et.ID)
	if err != nil {
		h.logger.ErrorContext(ctx, "staff booking: hosts", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	candidates, required, optional := resolveBookingHostPool(hosts, et.RoutingMode)
	switch {
	case et.RoutingMode == "round_robin" && slices.Contains(candidates, req.HostID):
		candidates = []string{req.HostID}
	case et.RoutingMode == "round_robin" && slices.Contains(required, req.HostID):
		// A fixed host of the rotation: keep the rotation pick, they attend anyway.
	case slices.Contains(candidates, req.HostID):
		candidates = append([]string{req.HostID}, slices.DeleteFunc(slices.Clone(candidates), func(id string) bool { return id == req.HostID })...)
	default:
		h.writeError(w, http.StatusBadRequest, "this host is not a required or rotation host of that event type")
		return
	}
	if len(candidates) == 0 {
		h.writeError(w, http.StatusConflict, "no active host can take this booking")
		return
	}

	if c := pastConflict(start); c != nil {
		h.writeConflicts(w, c)
		return
	}
	// Who will attend, for the warnings. A fixed host picked on a round-robin leaves the
	// rotation pick to Create, which still refuses a double booking unless forced.
	attending := slices.Clone(required)
	if et.RoutingMode != "round_robin" || len(candidates) == 1 {
		attending = append(attending, candidates...)
	}
	if !req.Force {
		conflicts, err := h.staffConflicts(ctx, et.ID, attending, start, end, "")
		if err != nil {
			h.logger.ErrorContext(ctx, "staff booking: conflicts", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if len(conflicts) > 0 {
			h.writeConflicts(w, conflicts)
			return
		}
	}
	// Optional hosts join only when nothing is in their way; they never raise a warning.
	var freeOptional []string
	for _, id := range optional {
		if slices.Contains(attending, id) {
			continue
		}
		if cs, err := h.staffConflicts(ctx, et.ID, []string{id}, start, end, ""); err == nil && len(cs) == 0 {
			freeOptional = append(freeOptional, id)
		}
	}

	b, err := h.bookingSvc.Create(ctx, booking.CreateParams{
		EventTypeID:   et.ID,
		HostIDs:       candidates,
		RoutingMode:   et.RoutingMode,
		RRStrategy:    et.RRStrategy,
		RequiredHosts: required,
		OptionalHosts: freeOptional,
		StartAt:       start,
		EndAt:         end,
		LocationValue: locValue,
		LocationType:  et.LocationType,
		Organizer: booking.Attendee{
			Name: req.Name, Email: email, IANATimezone: req.Timezone, Locale: loc.Code,
		},
		Guests:         guests,
		InviteDelivery: et.InviteDelivery,
		AllowOverlap:   req.Force,
	})
	if err != nil {
		switch {
		case errors.Is(err, booking.ErrDoubleBooked) && req.Force:
			h.writeConflicts(w, sameStartConflict(req.HostID, h.userName(ctx, req.HostID)))
		case errors.Is(err, booking.ErrDoubleBooked):
			h.writeConflicts(w, []staffConflict{{HostID: req.HostID, HostName: h.userName(ctx, req.HostID), Kind: "booking", Detail: "overlaps another appointment"}})
		default:
			h.logger.ErrorContext(ctx, "staff booking: create", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	h.logger.InfoContext(ctx, "staff booking created", "booking_id", b.ID, "by", actor.ID, "host", req.HostID, "forced", req.Force)

	bj := toBookingJSON(b)
	for _, hd := range h.displayHostsForBooking(ctx, b.ID) {
		bj.Hosts = append(bj.Hosts, hostBrief{ID: hd.ID, Name: hd.Name, AvatarURL: hd.AvatarURL})
	}
	h.writeJSON(w, http.StatusCreated, bj)

	go h.dispatchBookingConfirmation(b, bookingConfirmationInput{ // #nosec G118 -- its own context, as for CreateBooking
		EventTypeName:     et.Name,
		EventTypeSlug:     slug,
		LocationType:      et.LocationType,
		OrganizerName:     req.Name,
		OrganizerEmail:    email,
		OrganizerTimezone: req.Timezone,
		OrganizerLocale:   loc.Code,
	})
}

// loadManagedBooking loads booking id for a staff action and checks the actor manages its
// primary host. Writes the response and returns nil on failure (404 when not visible).
func (h *Handler) loadManagedBooking(w http.ResponseWriter, r *http.Request, actor AuthUser) *booking.Booking {
	b, err := h.bookingSvc.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, booking.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		h.writeError(w, http.StatusNotFound, "booking not found")
		return nil
	}
	if err != nil {
		h.logger.ErrorContext(r.Context(), "staff booking: load", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return nil
	}
	ok, err := h.canManageHost(r.Context(), actor, b.HostID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "staff booking: access", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return nil
	}
	if !ok {
		h.writeError(w, http.StatusNotFound, "booking not found")
		return nil
	}
	return b
}

// MoveStaffBooking handles PATCH /v1/staff/bookings/{id}: a drag or resize on the
// Schedule page. end_at defaults to start_at plus the booking's current length.
func (h *Handler) MoveStaffBooking(w http.ResponseWriter, r *http.Request) {
	actor, _ := userFromContext(r.Context())
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req struct {
		StartAt string `json:"start_at"`
		EndAt   string `json:"end_at"`
		Force   bool   `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.StartAt == "" {
		h.writeError(w, http.StatusBadRequest, "start_at is required")
		return
	}
	b := h.loadManagedBooking(w, r, actor)
	if b == nil {
		return
	}
	if b.Status == "cancelled" {
		h.writeError(w, http.StatusConflict, "this booking has been cancelled")
		return
	}
	start, err := time.Parse(time.RFC3339, req.StartAt)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "start_at must be RFC3339")
		return
	}
	start = start.UTC()
	end := start.Add(b.EndAt.Sub(b.StartAt))
	if req.EndAt != "" {
		if end, err = time.Parse(time.RFC3339, req.EndAt); err != nil {
			h.writeError(w, http.StatusBadRequest, "end_at must be RFC3339")
			return
		}
		end = end.UTC()
	}
	if !end.After(start) || end.Sub(start) > 24*time.Hour {
		h.writeError(w, http.StatusBadRequest, "end_at must be after start_at and at most 24 hours later")
		return
	}
	if start.Equal(b.StartAt) && end.Equal(b.EndAt) {
		h.writeJSON(w, http.StatusOK, toBookingJSON(b))
		return
	}
	if c := pastConflict(start); c != nil {
		h.writeConflicts(w, c)
		return
	}
	hosts, err := h.assignedHosts(ctx, b.ID)
	if err != nil {
		h.logger.ErrorContext(ctx, "staff move: hosts", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	hostIDs := []string{b.HostID}
	for _, ah := range hosts {
		if !slices.Contains(hostIDs, ah.UserID) {
			hostIDs = append(hostIDs, ah.UserID)
		}
	}
	if !req.Force {
		conflicts, err := h.staffConflicts(ctx, b.EventTypeID, hostIDs, start, end, b.ID)
		if err != nil {
			h.logger.ErrorContext(ctx, "staff move: conflicts", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if len(conflicts) > 0 {
			h.writeConflicts(w, conflicts)
			return
		}
	}

	previousStart, previousEnd := b.StartAt, b.EndAt
	move := h.bookingSvc.Reschedule
	if req.Force {
		move = h.bookingSvc.RescheduleAllowOverlap
	}
	updated, err := move(ctx, b.ID, start, end)
	switch {
	case errors.Is(err, booking.ErrDoubleBooked) && req.Force:
		h.writeConflicts(w, sameStartConflict(b.HostID, h.userName(ctx, b.HostID)))
		return
	case errors.Is(err, booking.ErrDoubleBooked):
		h.writeConflicts(w, []staffConflict{{HostID: b.HostID, HostName: h.userName(ctx, b.HostID), Kind: "booking", Detail: "overlaps another appointment"}})
		return
	case errors.Is(err, booking.ErrAlreadyCancelled):
		h.writeError(w, http.StatusConflict, "this booking has been cancelled")
		return
	case err != nil:
		h.logger.ErrorContext(ctx, "staff move: reschedule", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.logger.InfoContext(ctx, "staff booking moved", "booking_id", b.ID, "by", actor.ID, "forced", req.Force)
	h.writeJSON(w, http.StatusOK, toBookingJSON(updated))
	go h.rescheduleSideEffects(*updated, updated.EventTypeID, previousStart, previousEnd) // #nosec G118 -- its own context, as for RescheduleBooking
}

// CancelStaffBooking handles POST /v1/staff/bookings/{id}/cancel.
func (h *Handler) CancelStaffBooking(w http.ResponseWriter, r *http.Request) {
	actor, _ := userFromContext(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	b := h.loadManagedBooking(w, r, actor)
	if b == nil {
		return
	}
	who := strings.TrimSpace(actor.Name)
	if who == "" {
		who = actor.Email
	}
	if err := h.bookingSvc.CancelByID(r.Context(), b.ID, "Cancelled by "+who); err != nil {
		switch {
		case errors.Is(err, booking.ErrAlreadyCancelled):
			h.writeError(w, http.StatusConflict, "booking already cancelled")
		case errors.Is(err, booking.ErrNotFound):
			h.writeError(w, http.StatusNotFound, "booking not found")
		default:
			h.logger.ErrorContext(r.Context(), "staff cancel", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	cancelled, err := h.bookingSvc.Get(r.Context(), b.ID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "staff cancel: reload", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.logger.InfoContext(r.Context(), "staff booking cancelled", "booking_id", b.ID, "by", actor.ID)
	h.writeJSON(w, http.StatusOK, toBookingJSON(cancelled))
	go h.cancelSideEffects(*cancelled) // #nosec G118 -- its own context, as for CancelBooking
}
