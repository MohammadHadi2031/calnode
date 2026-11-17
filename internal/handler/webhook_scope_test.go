package handler_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/calnode/calnode/internal/handler"
)

// Webhook scope ("own" | "all", wisegrid fork): only an admin may widen a webhook to
// every booking, and the check applies on change, not on mention.

func createScopedWebhook(t *testing.T, h *handler.Handler, apiKey, scope string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	body := `{"url":"https://example.com/hook","events":["booking.created"]`
	if scope != "" {
		body += `,"scope":"` + scope + `"`
	}
	body += `}`
	rec := httptest.NewRecorder()
	h.RequireAuth(h.CreateWebhook)(rec, authReq(http.MethodPost, "/v1/webhooks", body, apiKey))
	if rec.Code != http.StatusCreated {
		return rec, ""
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	id, _ := resp["id"].(string)
	return rec, id
}

func patchWebhook(h *handler.Handler, apiKey, id, body string) *httptest.ResponseRecorder {
	req := authReq(http.MethodPatch, "/v1/webhooks/"+id, body, apiKey)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.PatchWebhook)(rec, req)
	return rec
}

func listedScope(t *testing.T, h *handler.Handler, apiKey, id string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.RequireAuth(h.ListWebhooks)(rec, authReq(http.MethodGet, "/v1/webhooks", "", apiKey))
	var resp struct {
		Items []struct {
			ID    string `json:"id"`
			Scope string `json:"scope"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	for _, it := range resp.Items {
		if it.ID == id {
			return it.Scope
		}
	}
	t.Fatalf("webhook %s not listed", id)
	return ""
}

func demote(t *testing.T, database *sql.DB, userID string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE users SET is_admin = 0, is_owner = 0 WHERE id = ?`, userID); err != nil {
		t.Fatalf("demote: %v", err)
	}
}

func TestWebhookScope_defaultsToOwn(t *testing.T) {
	h, apiKey, _ := setupWorkspace(t)
	rec, id := createScopedWebhook(t, h, apiKey, "")
	if id == "" {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if got := listedScope(t, h, apiKey, id); got != "own" {
		t.Errorf("scope = %q; want own", got)
	}
}

func TestWebhookScope_adminCanCreateAll(t *testing.T) {
	h, apiKey, _ := setupWorkspace(t)
	rec, id := createScopedWebhook(t, h, apiKey, "all")
	if id == "" {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if got := listedScope(t, h, apiKey, id); got != "all" {
		t.Errorf("scope = %q; want all", got)
	}
}

func TestWebhookScope_unknownScopeRejected(t *testing.T) {
	h, apiKey, _ := setupWorkspace(t)
	if rec, _ := createScopedWebhook(t, h, apiKey, "everyone"); rec.Code != http.StatusBadRequest {
		t.Errorf("create status = %d; want 400", rec.Code)
	}
	_, id := createScopedWebhook(t, h, apiKey, "")
	if rec := patchWebhook(h, apiKey, id, `{"scope":"everyone"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("patch status = %d; want 400", rec.Code)
	}
}

func TestWebhookScope_memberCannotWidenToAll(t *testing.T) {
	h, database, apiKey, userID := setupWorkspaceWithDB(t)
	_, id := createScopedWebhook(t, h, apiKey, "")
	demote(t, database, userID)

	if rec, _ := createScopedWebhook(t, h, apiKey, "all"); rec.Code != http.StatusBadRequest {
		t.Errorf("member create all: status = %d; want 400", rec.Code)
	}
	if rec := patchWebhook(h, apiKey, id, `{"scope":"all"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("member patch to all: status = %d; want 400", rec.Code)
	}
	if got := listedScope(t, h, apiKey, id); got != "own" {
		t.Errorf("scope after refused patch = %q; want own", got)
	}
}

// A demoted admin's webhook keeps its stored "all" (Enqueue stops honouring it), and
// the owner can still save a form that repeats it, or narrow it.
func TestWebhookScope_demotedOwnerCanStillEditUnchangedScope(t *testing.T) {
	h, database, apiKey, userID := setupWorkspaceWithDB(t)
	rec, id := createScopedWebhook(t, h, apiKey, "all")
	if id == "" {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	demote(t, database, userID)

	if rec := patchWebhook(h, apiKey, id, `{"scope":"all","is_active":false}`); rec.Code != http.StatusNoContent {
		t.Fatalf("patch repeating stored scope: status = %d; want 204 — %s", rec.Code, rec.Body.String())
	}
	if rec := patchWebhook(h, apiKey, id, `{"scope":"own"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("narrow to own: status = %d; want 204 — %s", rec.Code, rec.Body.String())
	}
	if got := listedScope(t, h, apiKey, id); got != "own" {
		t.Errorf("scope = %q; want own", got)
	}
}

func TestWebhookScope_patchUnknownWebhook404(t *testing.T) {
	h, apiKey, _ := setupWorkspace(t)
	if rec := patchWebhook(h, apiKey, "nope", `{"scope":"all"}`); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}
