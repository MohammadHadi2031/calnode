package handler

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The host's own reminder switch round-trips through /v1/users/me, defaulting to on, and
// patching other preferences leaves it alone.
func TestHostReminderPref_roundTrip(t *testing.T) {
	f := newInviteFixture(t, true)
	get := func() map[string]any {
		t.Helper()
		rec := f.do(t, f.h.GetMe, http.MethodGet, "/v1/users/me", "")
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	if v, ok := get()["notify_host_reminder"].(bool); !ok || !v {
		t.Fatalf("default notify_host_reminder = %v; want true", get()["notify_host_reminder"])
	}
	rec := f.do(t, f.h.PatchMe, http.MethodPatch, "/v1/users/me", `{"notify_host_reminder":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d — %s", rec.Code, rec.Body.String())
	}
	var patched map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &patched)
	if patched["notify_host_reminder"] != false {
		t.Errorf("patch response notify_host_reminder = %v; want false", patched["notify_host_reminder"])
	}
	f.do(t, f.h.PatchMe, http.MethodPatch, "/v1/users/me", `{"notify_host_booking":false}`)
	if get()["notify_host_reminder"] != false {
		t.Error("patching another preference reset notify_host_reminder")
	}
}
