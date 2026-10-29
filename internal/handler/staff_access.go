package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Fork (wisegrid): who may manage whose calendar from the Schedule page. A host manages
// their own; an admin manages everyone; a secretary manages the hosts an admin assigned
// them in secretary_hosts (migration 90002). See docs/fork-staff-calendar.md.

// canManageHost reports whether actor may create, move and cancel hostID's appointments
// and unavailable blocks.
func (h *Handler) canManageHost(ctx context.Context, actor AuthUser, hostID string) (bool, error) {
	if hostID == "" {
		return false, nil
	}
	if actor.ID == hostID || actor.IsAdmin {
		return true, nil
	}
	var n int
	err := h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM secretary_hosts WHERE secretary_id = ? AND host_id = ?`, actor.ID, hostID).Scan(&n)
	return n > 0, err
}

// secretaryHostIDs returns the hosts assigned to secretaryID, oldest assignment first.
func (h *Handler) secretaryHostIDs(ctx context.Context, secretaryID string) ([]string, error) {
	rows, err := h.db.QueryContext(ctx,
		`SELECT host_id FROM secretary_hosts WHERE secretary_id = ? ORDER BY created_at, host_id`, secretaryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// managedHost is one host on the Schedule page's host list.
type managedHost struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Timezone  string `json:"timezone"`
	AvatarURL string `json:"avatar_url,omitempty"`
	Self      bool   `json:"self"`
}

// managedHosts lists the active users actor may manage: themselves first, then everyone
// else by name (every member for an admin, the assigned hosts for a secretary).
func (h *Handler) managedHosts(ctx context.Context, actor AuthUser) ([]managedHost, error) {
	q := `SELECT id, name, email, iana_timezone, COALESCE(avatar_url, '') FROM users
		WHERE archived_at IS NULL AND (id = ? OR id IN (SELECT host_id FROM secretary_hosts WHERE secretary_id = ?))
		ORDER BY id != ?, name COLLATE NOCASE, email`
	args := []any{actor.ID, actor.ID, actor.ID}
	if actor.IsAdmin {
		q = `SELECT id, name, email, iana_timezone, COALESCE(avatar_url, '') FROM users
			WHERE archived_at IS NULL ORDER BY id != ?, name COLLATE NOCASE, email`
		args = []any{actor.ID}
	}
	rows, err := h.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []managedHost
	for rows.Next() {
		var m managedHost
		if err := rows.Scan(&m.ID, &m.Name, &m.Email, &m.Timezone, &m.AvatarURL); err != nil {
			return nil, err
		}
		m.Self = m.ID == actor.ID
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListSecretaryAssignments handles GET /v1/secretaries (admin): every secretary→host pair.
func (h *Handler) ListSecretaryAssignments(w http.ResponseWriter, r *http.Request) {
	actor, ok := userFromContext(r.Context())
	if !ok || !actor.IsAdmin {
		h.writeError(w, http.StatusForbidden, "admin access required")
		return
	}
	rows, err := h.db.QueryContext(r.Context(), `
		SELECT s.secretary_id, s.host_id FROM secretary_hosts s
		JOIN users su ON su.id = s.secretary_id
		JOIN users hu ON hu.id = s.host_id
		ORDER BY su.name COLLATE NOCASE, hu.name COLLATE NOCASE`)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "list secretaries", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	type pair struct {
		SecretaryID string `json:"secretary_id"`
		HostID      string `json:"host_id"`
	}
	out := []pair{}
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.SecretaryID, &p.HostID); err != nil {
			h.logger.ErrorContext(r.Context(), "list secretaries: scan", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		h.logger.ErrorContext(r.Context(), "list secretaries: rows", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"assignments": out})
}

// SetSecretaryHosts handles PUT /v1/users/{id}/secretary-hosts (admin). Body
// {"host_ids": [...]} replaces the set of hosts user {id} is secretary for; an empty list
// makes them an ordinary member again. Their own id is ignored, unknown or archived hosts
// are refused.
func (h *Handler) SetSecretaryHosts(w http.ResponseWriter, r *http.Request) {
	actor, ok := userFromContext(r.Context())
	if !ok || !actor.IsAdmin {
		h.writeError(w, http.StatusForbidden, "admin access required")
		return
	}
	secretaryID := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req struct {
		HostIDs []string `json:"host_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	var archived sql.NullString
	err := h.db.QueryRowContext(r.Context(), `SELECT archived_at FROM users WHERE id = ?`, secretaryID).Scan(&archived)
	if errors.Is(err, sql.ErrNoRows) {
		h.writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		h.logger.ErrorContext(r.Context(), "set secretary hosts: load user", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if archived.Valid && len(req.HostIDs) > 0 {
		h.writeError(w, http.StatusBadRequest, "an archived member cannot be a secretary")
		return
	}

	seen := map[string]bool{}
	hostIDs := []string{}
	for _, id := range req.HostIDs {
		id = strings.TrimSpace(id)
		if id == "" || id == secretaryID || seen[id] {
			continue
		}
		seen[id] = true
		hostIDs = append(hostIDs, id)
	}

	tx, err := h.db.BeginTx(r.Context(), nil)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "set secretary hosts: begin", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback() //nolint:errcheck
	for _, id := range hostIDs {
		var n int
		if err := tx.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM users WHERE id = ? AND archived_at IS NULL`, id).Scan(&n); err != nil {
			h.logger.ErrorContext(r.Context(), "set secretary hosts: check host", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if n == 0 {
			h.writeError(w, http.StatusBadRequest, "unknown or archived host: "+id)
			return
		}
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM secretary_hosts WHERE secretary_id = ?`, secretaryID); err != nil {
		h.logger.ErrorContext(r.Context(), "set secretary hosts: clear", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	for _, id := range hostIDs {
		if _, err := tx.ExecContext(r.Context(),
			`INSERT INTO secretary_hosts (secretary_id, host_id) VALUES (?, ?)`, secretaryID, id); err != nil {
			h.logger.ErrorContext(r.Context(), "set secretary hosts: insert", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		h.logger.ErrorContext(r.Context(), "set secretary hosts: commit", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"host_ids": hostIDs})
}
