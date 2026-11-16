package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/calnode/calnode/internal/calendar/microsoft"
)

// Workspace Teams account (wisegrid fork): one Microsoft 365 mailbox, set here by an
// admin, organizes the Teams meeting for every host without a Microsoft work account of
// their own. See microsoft.Workspace for why it uses the app's own credentials, and
// docs/fork-teams-setup.md for the Microsoft-side setup it needs.

// SetTeamsUnavailable records why this server cannot use a workspace Teams account (its
// Microsoft app is missing, or MICROSOFT_TENANT is not one organization). Boot only.
func (h *Handler) SetTeamsUnavailable(reason string) { h.teamsUnavailable = reason }

// teamsWorkspace is the workspace organizer, or nil when this server has none.
func (h *Handler) teamsWorkspace() *microsoft.Workspace {
	gc := h.getCal()
	if gc == nil {
		return nil
	}
	ws, _ := gc.Workspace().(*microsoft.Workspace)
	return ws
}

// teamsSettingsPayload is GET /v1/settings/teams.
func (h *Handler) teamsSettingsPayload(ctx context.Context) (map[string]any, error) {
	ws := h.teamsWorkspace()
	out := map[string]any{"available": ws != nil, "mailbox": ""}
	if ws == nil {
		reason := h.teamsUnavailable
		if reason == "" {
			reason = "Microsoft isn't configured on this server."
		}
		out["reason"] = reason
		return out, nil
	}
	mb, err := ws.Mailbox(ctx)
	if err != nil {
		return nil, err
	}
	out["mailbox"] = mb
	return out, nil
}

// GetTeamsSettings handles GET /v1/settings/teams (admin only).
func (h *Handler) GetTeamsSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	out, err := h.teamsSettingsPayload(r.Context())
	if err != nil {
		h.logger.ErrorContext(r.Context(), "teams settings: query", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.writeJSON(w, http.StatusOK, out)
}

// normalizeTeamsMailbox validates a mailbox address and returns it lowercased; "" stays "".
func normalizeTeamsMailbox(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", true
	}
	a, err := mail.ParseAddress(raw)
	if err != nil || a.Address != raw {
		return "", false // a bare address only, not `"Name" <addr>`
	}
	return strings.ToLower(a.Address), true
}

// PatchTeamsSettings handles PATCH /v1/settings/teams (admin only): {"mailbox": "..."}.
// An empty mailbox turns the workspace account off. Saving does not require the mailbox
// to pass Test: the Microsoft side is often still being set up when the address is known,
// and bookings fall back to a host's own account or the manual link until it works.
//
// Existing bookings are unaffected by a change: each one recorded the mailbox that holds
// its event, and reschedules and cancels go there.
func (h *Handler) PatchTeamsSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	if h.demoMode {
		h.writeError(w, http.StatusServiceUnavailable, "not available in the demo")
		return
	}
	if h.teamsWorkspace() == nil {
		h.writeError(w, http.StatusConflict, h.teamsUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req struct {
		Mailbox string `json:"mailbox"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	mb, ok := normalizeTeamsMailbox(req.Mailbox)
	if !ok {
		h.writeError(w, http.StatusBadRequest, "enter the account's email address, e.g. appointments@yourcompany.onmicrosoft.com")
		return
	}
	if _, err := h.db.ExecContext(r.Context(),
		`UPDATE server_settings SET teams_mailbox = ?, updated_at = datetime('now') WHERE id = 1`, mb); err != nil {
		h.logger.ErrorContext(r.Context(), "teams settings: update", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.GetTeamsSettings(w, r)
}

// TestTeamsSettings handles POST /v1/settings/teams/test (admin only): {"mailbox": "..."},
// or the saved mailbox when omitted. Read-only on the Microsoft side: it creates nothing.
func (h *Handler) TestTeamsSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	ws := h.teamsWorkspace()
	if ws == nil {
		h.writeError(w, http.StatusConflict, h.teamsUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req struct {
		Mailbox string `json:"mailbox"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			h.writeError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
	}
	mb, ok := normalizeTeamsMailbox(req.Mailbox)
	if !ok {
		h.writeError(w, http.StatusBadRequest, "enter the account's email address")
		return
	}
	if mb == "" {
		var err error
		if mb, err = ws.Mailbox(r.Context()); err != nil {
			h.logger.ErrorContext(r.Context(), "teams settings: query", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if mb == "" {
		h.writeError(w, http.StatusBadRequest, "enter the account's email address first")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	h.writeJSON(w, http.StatusOK, ws.Check(ctx, mb))
}
