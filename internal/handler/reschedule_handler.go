package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/calnode/calnode/internal/booking"
)

// RescheduleBooking handles PATCH /v1/bookings/{id}/reschedule (admin — host only).
// Body: {"start_at":"<RFC3339>"}
// end_at is computed from the event type's duration_minutes.
func (h *Handler) RescheduleBooking(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)

	var req struct {
		StartAt string `json:"start_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.StartAt == "" {
		h.writeError(w, http.StatusBadRequest, "start_at is required")
		return
	}
	newStart, err := time.Parse(time.RFC3339, req.StartAt)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "start_at must be RFC3339 (e.g. 2026-06-15T09:00:00Z)")
		return
	}
	if newStart.Before(time.Now()) {
		h.writeError(w, http.StatusBadRequest, "start_at cannot be in the past")
		return
	}

	// Load booking + event type in one query to check ownership and get duration.
	var hostID, startStr, endStr, status, etSlug, etID string
	var durMins int
	err = h.db.QueryRowContext(r.Context(), `
		SELECT b.host_id, b.start_at, b.end_at, b.status, et.duration_minutes, et.slug, et.id
		FROM bookings b JOIN event_types et ON et.id = b.event_type_id
		WHERE b.id = ?`, id).
		Scan(&hostID, &startStr, &endStr, &status, &durMins, &etSlug, &etID)
	if errors.Is(err, sql.ErrNoRows) {
		h.writeError(w, http.StatusNotFound, "booking not found")
		return
	}
	if err != nil {
		h.logger.ErrorContext(r.Context(), "reschedule booking: load", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Who may reschedule: any host with a seat on the booking (co-hosts included), an
	// admin, or the primary host's secretary (mayManageBooking), the same people the
	// Bookings page shows the booking to. It used to be the primary host alone, so an
	// admin's, secretary's or co-host's reschedule button answered "booking not found".
	// Return 404 for unauthorized access to avoid leaking booking IDs.
	if ok, err := h.mayManageBooking(r.Context(), user, id); err != nil {
		h.logger.ErrorContext(r.Context(), "reschedule booking: access", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	} else if !ok {
		h.writeError(w, http.StatusNotFound, "booking not found")
		return
	}
	if status == "cancelled" {
		h.writeError(w, http.StatusConflict, "this booking has been cancelled")
		return
	}

	previousStart, err := time.Parse(time.RFC3339Nano, startStr)
	if err != nil {
		// Fallback for RFC3339 without nanoseconds.
		previousStart, err = time.Parse(time.RFC3339, startStr)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "reschedule booking: parse start_at", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	previousEnd, err := time.Parse(time.RFC3339Nano, endStr)
	if err != nil {
		previousEnd, err = time.Parse(time.RFC3339, endStr)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "reschedule booking: parse end_at", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	// Keep the booking's own length. A staff appointment can be longer or shorter than
	// the event type (dragged or resized on the Schedule page); rescheduling used to
	// reset it to the event type's duration without saying so.
	length := previousEnd.Sub(previousStart)
	if length <= 0 {
		length = time.Duration(durMins) * time.Minute
	}
	newEnd := newStart.Add(length)

	if err := h.validateRescheduleTime(r.Context(), id, etID, hostID, newStart, newEnd); err != nil {
		if errors.Is(err, errSlotUnavailable) {
			h.writeError(w, http.StatusConflict, "this slot is no longer available")
			return
		}
		h.logger.ErrorContext(r.Context(), "reschedule booking: validate time", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	updated, err := h.bookingSvc.Reschedule(r.Context(), id, newStart, newEnd)
	if errors.Is(err, booking.ErrDoubleBooked) {
		h.writeError(w, http.StatusConflict, "that time slot is no longer available")
		return
	}
	if errors.Is(err, booking.ErrAlreadyCancelled) {
		h.writeError(w, http.StatusConflict, "this booking has been cancelled")
		return
	}
	if errors.Is(err, booking.ErrNotFound) {
		h.writeError(w, http.StatusNotFound, "booking not found")
		return
	}
	if err != nil {
		h.logger.ErrorContext(r.Context(), "reschedule booking: update", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	h.writeJSON(w, http.StatusOK, toBookingJSON(updated))

	// Side effects (calendar move, Zoom update, emails, webhook, reminders) are shared
	// with the manage-token reschedule flow — see rescheduleSideEffects in
	// manage_handler.go. etSlug is unused here now; it's re-derived inside the helper
	// via loadCancellationData's own join, which is equivalent.
	go h.rescheduleSideEffects(*updated, etID, previousStart, previousEnd) // #nosec G118 -- deliberately its own context.Background(); see rescheduleSideEffects' doc comment
}
