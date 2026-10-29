package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/slots"
	"github.com/calnode/calnode/internal/uid"
)

// Fork (wisegrid): unavailable blocks - exact UTC ranges a host cannot be booked, set
// from the Schedule page (migration 90002, docs/fork-staff-calendar.md). They remove
// slots (hostAvailability adds them to busy), fail hostAvailableAt for public bookings,
// and with sync_to_calendar are mirrored as a busy event on the host's destination
// calendar.

const maxBlockTitle = 200

// blockCalendarTimeout bounds the calendar write a block save waits for; the block itself
// is saved either way and reports synced=false when the write did not land.
var blockCalendarTimeout = 15 * time.Second

type unavailableBlock struct {
	ID             string    `json:"id"`
	HostID         string    `json:"host_id"`
	StartAt        time.Time `json:"start_at"`
	EndAt          time.Time `json:"end_at"`
	Title          string    `json:"title"`
	SyncToCalendar bool      `json:"sync_to_calendar"`
	Synced         bool      `json:"synced"`
	SyncError      string    `json:"sync_error,omitempty"`

	extEventID, extCalendarID, extProvider string
}

const blockColumns = `id, user_id, start_at, end_at, title, sync_to_calendar,
	external_event_id, external_calendar_id, external_provider`

func scanBlock(s rowScanner) (*unavailableBlock, error) {
	var b unavailableBlock
	var start, end string
	if err := s.Scan(&b.ID, &b.HostID, &start, &end, &b.Title, &b.SyncToCalendar,
		&b.extEventID, &b.extCalendarID, &b.extProvider); err != nil {
		return nil, err
	}
	var err error
	if b.StartAt, err = time.Parse(time.RFC3339Nano, start); err != nil {
		return nil, err
	}
	if b.EndAt, err = time.Parse(time.RFC3339Nano, end); err != nil {
		return nil, err
	}
	b.Synced = b.extEventID != ""
	return &b, nil
}

func blockTime(t time.Time) string { return t.UTC().Truncate(time.Second).Format(time.RFC3339) }

// hostBlocked reports whether an unavailable block of userID overlaps [start, end).
func (h *Handler) hostBlocked(ctx context.Context, userID string, start, end time.Time) (bool, error) {
	var n int
	err := h.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM unavailable_blocks
		WHERE user_id = ? AND start_at < ? AND end_at > ?`,
		userID, end.UTC().Format(time.RFC3339Nano), start.UTC().Format(time.RFC3339Nano)).Scan(&n)
	return n > 0, err
}

// blocksBetween returns userID's blocks overlapping [from, to), by start.
func (h *Handler) blocksBetween(ctx context.Context, userID string, from, to time.Time) ([]unavailableBlock, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT `+blockColumns+` FROM unavailable_blocks
		WHERE user_id = ? AND start_at < ? AND end_at > ? ORDER BY start_at`,
		userID, to.UTC().Format(time.RFC3339Nano), from.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []unavailableBlock
	for rows.Next() {
		b, err := scanBlock(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

func blockIntervals(blocks []unavailableBlock) []slots.Interval {
	out := make([]slots.Interval, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, slots.Interval{Start: b.StartAt, End: b.EndAt})
	}
	return out
}

// bookingOverlapConflicts lists hostID's non-cancelled bookings overlapping [start, end)
// as "booking" conflicts (excludeBookingID: the booking being moved).
func (h *Handler) bookingOverlapConflicts(ctx context.Context, hostID, hostName string, start, end time.Time, excludeBookingID string) ([]staffConflict, error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT b.start_at, COALESCE(a.name, '') FROM bookings b
		JOIN booking_hosts bh ON bh.booking_id = b.id
		LEFT JOIN booking_attendees a ON a.booking_id = b.id AND a.is_organizer = 1
		WHERE bh.user_id = ? AND b.status != 'cancelled' AND b.id != ?
		  AND b.start_at < ? AND b.end_at > ?
		ORDER BY b.start_at LIMIT 5`,
		hostID, excludeBookingID, end.UTC().Format(time.RFC3339Nano), start.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []staffConflict
	for rows.Next() {
		var s, who string
		if err := rows.Scan(&s, &who); err != nil {
			return nil, err
		}
		detail := "overlaps another appointment"
		if who != "" {
			detail = "overlaps the appointment with " + who
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			detail += " at " + t.UTC().Format("2006-01-02 15:04 UTC")
		}
		out = append(out, staffConflict{HostID: hostID, HostName: hostName, Kind: "booking", Detail: detail})
	}
	return out, rows.Err()
}

// writeBlockEvent mirrors b on the host's destination calendar - creating, moving or
// re-creating its busy event as needed - or removes it when sync is off. Errors are
// reported on b.SyncError (the block itself is already saved).
func (h *Handler) writeBlockEvent(ctx context.Context, b *unavailableBlock, titleChanged bool) {
	gc := h.getCal()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), blockCalendarTimeout)
	defer cancel()

	if b.extEventID != "" && (!b.SyncToCalendar || titleChanged || gc == nil) {
		if gc != nil {
			if err := gc.CancelEvent(ctx, b.HostID, b.extCalendarID, b.extEventID, b.extProvider); err != nil {
				h.logger.WarnContext(ctx, "unavailable block: delete calendar event", "block", b.ID, "error", err)
				b.SyncError = "could not remove the event from the host's calendar"
				return // keep the ids so a later save or delete retries
			}
		}
		b.extEventID, b.extCalendarID, b.extProvider = "", "", ""
		h.storeBlockExternal(ctx, b)
	}
	if !b.SyncToCalendar || gc == nil {
		b.Synced = b.extEventID != ""
		return
	}

	if b.extEventID != "" {
		if err := gc.UpdateEvent(ctx, b.HostID, b.extCalendarID, b.extEventID, b.extProvider, b.StartAt, b.EndAt); err != nil {
			h.logger.WarnContext(ctx, "unavailable block: move calendar event", "block", b.ID, "error", err)
			b.SyncError = "could not update the event on the host's calendar"
		}
		b.Synced = true
		return
	}

	title := strings.TrimSpace(b.Title)
	if title == "" {
		title = "Unavailable"
	}
	eventID, _, calID, provider, err := gc.CreateEvent(ctx, b.HostID, calendar.CreateEventParams{
		Summary:     title,
		Description: "Blocked in Calnode: no bookings during this time.",
		Start:       b.StartAt,
		End:         b.EndAt,
	})
	switch {
	case err != nil:
		h.logger.WarnContext(ctx, "unavailable block: create calendar event", "block", b.ID, "error", err)
		b.SyncError = "could not add the event to the host's calendar"
	case eventID == "":
		b.SyncError = "the host has no calendar connected to write to"
	default:
		b.extEventID, b.extCalendarID, b.extProvider = eventID, calID, provider
		h.storeBlockExternal(ctx, b)
	}
	b.Synced = b.extEventID != ""
}

func (h *Handler) storeBlockExternal(ctx context.Context, b *unavailableBlock) {
	if _, err := h.db.ExecContext(ctx, `UPDATE unavailable_blocks
		SET external_event_id = ?, external_calendar_id = ?, external_provider = ? WHERE id = ?`,
		b.extEventID, b.extCalendarID, b.extProvider, b.ID); err != nil {
		h.logger.ErrorContext(ctx, "unavailable block: store calendar event", "block", b.ID, "error", err)
	}
}

// loadManagedBlock loads block id and checks the actor may manage its host. It writes the
// error response and returns nil on any failure (404 for a block the actor cannot see).
func (h *Handler) loadManagedBlock(w http.ResponseWriter, r *http.Request, actor AuthUser) *unavailableBlock {
	b, err := scanBlock(h.db.QueryRowContext(r.Context(),
		`SELECT `+blockColumns+` FROM unavailable_blocks WHERE id = ?`, r.PathValue("id")))
	if errors.Is(err, sql.ErrNoRows) {
		h.writeError(w, http.StatusNotFound, "unavailable block not found")
		return nil
	}
	if err != nil {
		h.logger.ErrorContext(r.Context(), "unavailable block: load", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return nil
	}
	ok, err := h.canManageHost(r.Context(), actor, b.HostID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "unavailable block: access", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return nil
	}
	if !ok {
		h.writeError(w, http.StatusNotFound, "unavailable block not found")
		return nil
	}
	return b
}

// blockConflicts are the appointments a block would cover. A block over an existing
// appointment does not cancel it, so staff confirm it (force) instead.
func (h *Handler) blockConflicts(ctx context.Context, hostID string, start, end time.Time) ([]staffConflict, error) {
	return h.bookingOverlapConflicts(ctx, hostID, h.userName(ctx, hostID), start, end, "")
}

func validBlockRange(start, end time.Time) string {
	switch {
	case !end.After(start):
		return "end_at must be after start_at"
	case end.Sub(start) > 62*24*time.Hour:
		return "an unavailable block can span at most 62 days"
	}
	return ""
}

// CreateUnavailableBlock handles POST /v1/unavailable-blocks.
func (h *Handler) CreateUnavailableBlock(w http.ResponseWriter, r *http.Request) {
	actor, _ := userFromContext(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var req struct {
		HostID         string `json:"host_id"`
		StartAt        string `json:"start_at"`
		EndAt          string `json:"end_at"`
		Title          string `json:"title"`
		SyncToCalendar *bool  `json:"sync_to_calendar"`
		Force          bool   `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.HostID == "" {
		req.HostID = actor.ID
	}
	start, err1 := time.Parse(time.RFC3339, req.StartAt)
	end, err2 := time.Parse(time.RFC3339, req.EndAt)
	if err1 != nil || err2 != nil {
		h.writeError(w, http.StatusBadRequest, "start_at and end_at must be RFC3339 (e.g. 2026-06-15T09:00:00Z)")
		return
	}
	start, end = start.UTC().Truncate(time.Second), end.UTC().Truncate(time.Second)
	if msg := validBlockRange(start, end); msg != "" {
		h.writeError(w, http.StatusBadRequest, msg)
		return
	}
	title := strings.TrimSpace(req.Title)
	if len(title) > maxBlockTitle {
		h.writeError(w, http.StatusBadRequest, "title is too long")
		return
	}
	if ok, err := h.canManageHost(r.Context(), actor, req.HostID); err != nil {
		h.logger.ErrorContext(r.Context(), "unavailable block: access", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	} else if !ok {
		h.writeError(w, http.StatusForbidden, "you cannot manage this host's calendar")
		return
	}
	if !req.Force {
		conflicts, err := h.blockConflicts(r.Context(), req.HostID, start, end)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "unavailable block: conflicts", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if len(conflicts) > 0 {
			h.writeConflicts(w, conflicts)
			return
		}
	}

	b := &unavailableBlock{
		ID: uid.New(), HostID: req.HostID, StartAt: start, EndAt: end, Title: title,
		SyncToCalendar: req.SyncToCalendar == nil || *req.SyncToCalendar,
	}
	if _, err := h.db.ExecContext(r.Context(), `INSERT INTO unavailable_blocks
		(id, user_id, start_at, end_at, title, sync_to_calendar, created_by) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		b.ID, b.HostID, blockTime(start), blockTime(end), b.Title, b.SyncToCalendar, actor.ID); err != nil {
		h.logger.ErrorContext(r.Context(), "unavailable block: insert", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if b.SyncToCalendar {
		h.writeBlockEvent(r.Context(), b, false)
	}
	h.writeJSON(w, http.StatusCreated, b)
}

// UpdateUnavailableBlock handles PATCH /v1/unavailable-blocks/{id}.
func (h *Handler) UpdateUnavailableBlock(w http.ResponseWriter, r *http.Request) {
	actor, _ := userFromContext(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var req struct {
		StartAt        *string `json:"start_at"`
		EndAt          *string `json:"end_at"`
		Title          *string `json:"title"`
		SyncToCalendar *bool   `json:"sync_to_calendar"`
		Force          bool    `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	b := h.loadManagedBlock(w, r, actor)
	if b == nil {
		return
	}
	start, end := b.StartAt, b.EndAt
	for _, f := range []struct {
		raw *string
		dst *time.Time
	}{{req.StartAt, &start}, {req.EndAt, &end}} {
		if f.raw == nil {
			continue
		}
		t, err := time.Parse(time.RFC3339, *f.raw)
		if err != nil {
			h.writeError(w, http.StatusBadRequest, "start_at and end_at must be RFC3339 (e.g. 2026-06-15T09:00:00Z)")
			return
		}
		*f.dst = t.UTC().Truncate(time.Second)
	}
	if msg := validBlockRange(start, end); msg != "" {
		h.writeError(w, http.StatusBadRequest, msg)
		return
	}
	title := b.Title
	if req.Title != nil {
		title = strings.TrimSpace(*req.Title)
		if len(title) > maxBlockTitle {
			h.writeError(w, http.StatusBadRequest, "title is too long")
			return
		}
	}
	timesChanged := !start.Equal(b.StartAt) || !end.Equal(b.EndAt)
	if timesChanged && !req.Force {
		conflicts, err := h.blockConflicts(r.Context(), b.HostID, start, end)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "unavailable block: conflicts", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if len(conflicts) > 0 {
			h.writeConflicts(w, conflicts)
			return
		}
	}
	titleChanged := title != b.Title
	syncBefore := b.SyncToCalendar
	b.StartAt, b.EndAt, b.Title = start, end, title
	if req.SyncToCalendar != nil {
		b.SyncToCalendar = *req.SyncToCalendar
	}
	if _, err := h.db.ExecContext(r.Context(), `UPDATE unavailable_blocks
		SET start_at = ?, end_at = ?, title = ?, sync_to_calendar = ?,
		    updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now') WHERE id = ?`,
		blockTime(start), blockTime(end), title, b.SyncToCalendar, b.ID); err != nil {
		h.logger.ErrorContext(r.Context(), "unavailable block: update", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Retry a sync that never landed on any save, not only when something changed.
	if timesChanged || titleChanged || syncBefore != b.SyncToCalendar || (b.SyncToCalendar && !b.Synced) {
		h.writeBlockEvent(r.Context(), b, titleChanged)
	}
	h.writeJSON(w, http.StatusOK, b)
}

// DeleteUnavailableBlock handles DELETE /v1/unavailable-blocks/{id}, removing the host's
// calendar event too. A failed calendar delete (e.g. a revoked token) is logged and does
// not keep the block: the leftover event is visible to the host, who can delete it.
func (h *Handler) DeleteUnavailableBlock(w http.ResponseWriter, r *http.Request) {
	actor, _ := userFromContext(r.Context())
	b := h.loadManagedBlock(w, r, actor)
	if b == nil {
		return
	}
	if b.extEventID != "" {
		b.SyncToCalendar = false
		h.writeBlockEvent(r.Context(), b, false)
	}
	if _, err := h.db.ExecContext(r.Context(), `DELETE FROM unavailable_blocks WHERE id = ?`, b.ID); err != nil {
		h.logger.ErrorContext(r.Context(), "unavailable block: delete", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
