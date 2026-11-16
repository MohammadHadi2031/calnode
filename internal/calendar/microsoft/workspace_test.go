package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/calendar"
)

type graphCall struct {
	method, path string
	body         map[string]any
}

// newTestWorkspace serves Graph from handler and records every call. The mailbox setting
// starts as mailbox.
func newTestWorkspace(t *testing.T, mailbox string, handler http.HandlerFunc) (*Workspace, *[]graphCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []graphCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		calls = append(calls, graphCall{r.Method, r.URL.Path, body})
		mu.Unlock()
		r.Body = io.NopCloser(strings.NewReader(string(b)))
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	db := newTestDB(t)
	ws := &Workspace{db: db, hc: srv.Client(), apiBase: srv.URL}
	setMailbox(t, ws, mailbox)
	return ws, &calls
}

func setMailbox(t *testing.T, ws *Workspace, mb string) {
	t.Helper()
	if _, err := ws.db.Exec(`UPDATE server_settings SET teams_mailbox = ? WHERE id = 1`, mb); err != nil {
		t.Fatal(err)
	}
}

func TestNewWorkspace_needsOneTenant(t *testing.T) {
	for _, tenant := range []string{"", "common", "Organizations", "consumers"} {
		if _, err := NewWorkspace(newTestDB(t), "id", "secret", tenant); !errors.Is(err, ErrWorkspaceTenant) {
			t.Errorf("tenant %q: err = %v; want ErrWorkspaceTenant", tenant, err)
		}
	}
	if _, err := NewWorkspace(newTestDB(t), "id", "secret", "contoso.onmicrosoft.com"); err != nil {
		t.Errorf("a named tenant: %v", err)
	}
}

// The event is created in the workspace mailbox as a Teams meeting, and the mailbox comes
// back as the calendar id so later calls can find it.
func TestWorkspaceCreateEvent(t *testing.T) {
	ws, calls := newTestWorkspace(t, "appts@contoso.test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"ev1","onlineMeeting":{"joinUrl":"https://teams.microsoft.com/l/meetup-join/x"}}`)
	})
	start := time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC)
	id, join, calID, err := ws.CreateEvent(context.Background(), "any-host", calendar.CreateEventParams{
		Summary: "Intro", Start: start, End: start.Add(30 * time.Minute),
		OrganizerName: "Booker", OrganizerEmail: "booker@x.test",
		Guests:  []calendar.Invitee{{Name: "Host", Email: "host@gmail.test"}},
		AddMeet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "ev1" || join == "" || calID != "appts@contoso.test" {
		t.Fatalf("got (%q, %q, %q)", id, join, calID)
	}
	c := (*calls)[0]
	if c.method != http.MethodPost || c.path != "/users/appts@contoso.test/events" {
		t.Fatalf("call = %s %s", c.method, c.path)
	}
	if c.body["isOnlineMeeting"] != true || c.body["onlineMeetingProvider"] != "teamsForBusiness" {
		t.Errorf("not a Teams meeting: %v", c.body)
	}
	if n := len(c.body["attendees"].([]any)); n != 2 {
		t.Errorf("attendees = %d; want booker + host", n)
	}
}

func TestWorkspaceCreateEvent_unconfiguredWritesNothing(t *testing.T) {
	ws, calls := newTestWorkspace(t, "", func(w http.ResponseWriter, r *http.Request) {})
	id, _, _, err := ws.CreateEvent(context.Background(), "h", calendar.CreateEventParams{AddMeet: true})
	if err != nil || id != "" || len(*calls) != 0 {
		t.Fatalf("id=%q err=%v calls=%d", id, err, len(*calls))
	}
}

// Changing the mailbox setting must not strand existing meetings: updates and cancels go
// to the mailbox recorded on the event.
func TestWorkspaceUpdateAndCancel_useRecordedMailbox(t *testing.T) {
	ws, calls := newTestWorkspace(t, "new@contoso.test", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	})
	ctx := context.Background()
	start := time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC)
	if err := ws.UpdateEvent(ctx, "h", "old@contoso.test", "ev1", start, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := ws.CancelEvent(ctx, "h", "old@contoso.test", "ev1"); err != nil {
		t.Fatal(err)
	}
	// A legacy row without a recorded mailbox uses the current setting.
	if err := ws.CancelEvent(ctx, "h", "", "ev2"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PATCH /users/old@contoso.test/events/ev1",
		"DELETE /users/old@contoso.test/events/ev1",
		"DELETE /users/new@contoso.test/events/ev2",
	}
	for i, w := range want {
		if got := (*calls)[i].method + " " + (*calls)[i].path; got != w {
			t.Errorf("call %d = %s; want %s", i, got, w)
		}
	}
}

func TestWorkspaceReplaceAttendee(t *testing.T) {
	ws, calls := newTestWorkspace(t, "appts@contoso.test", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"attendees":[
				{"emailAddress":{"address":"booker@x.test"},"type":"required"},
				{"emailAddress":{"address":"Old@Gmail.test"},"type":"required"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	})
	if err := ws.ReplaceAttendee(context.Background(), "appts@contoso.test", "ev1", "old@gmail.test",
		calendar.Invitee{Name: "New", Email: "new@gmail.test"}); err != nil {
		t.Fatal(err)
	}
	patch := (*calls)[1]
	if patch.method != http.MethodPatch {
		t.Fatalf("second call = %s", patch.method)
	}
	var got []string
	for _, a := range patch.body["attendees"].([]any) {
		got = append(got, a.(map[string]any)["emailAddress"].(map[string]any)["address"].(string))
	}
	if strings.Join(got, ",") != "booker@x.test,new@gmail.test" {
		t.Errorf("attendees = %v; want the booker kept, old host swapped for new", got)
	}
}

func TestWorkspaceIsNeverBusyOrADestination(t *testing.T) {
	ws, calls := newTestWorkspace(t, "appts@contoso.test", func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	busy, err := ws.FreeBusy(ctx, "h", time.Now(), time.Now().Add(time.Hour))
	if err != nil || busy != nil {
		t.Errorf("FreeBusy = %v, %v", busy, err)
	}
	if dest, _ := ws.HasDestination(ctx, "h"); dest {
		t.Error("HasDestination = true")
	}
	if len(*calls) != 0 {
		t.Errorf("made %d Graph calls", len(*calls))
	}
}

func TestWorkspaceCheck(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		ok       bool
		reach    bool
		contains string
	}{
		{"teams allowed", 200, `{"allowedOnlineMeetingProviders":["teamsForBusiness"]}`, true, true, ""},
		{"teams not yet allowed", 200, `{"allowedOnlineMeetingProviders":["unknown"]}`, false, true, "teamsForBusiness"},
		{"no permission", 403, `{"error":{"code":"ErrorAccessDenied","message":"Access is denied."}}`, false, false, "Calendars.ReadWrite"},
		{"no mailbox", 404, `{"error":{"code":"ErrorInvalidUser","message":"x"}}`, false, false, "No mailbox"},
		{"no exchange", 404, `{"error":{"code":"MailboxNotEnabledForRESTAPI","message":"x"}}`, false, false, "Exchange Online"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, calls := newTestWorkspace(t, "", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			got := ws.Check(context.Background(), "appts@contoso.test")
			if got.OK != tc.ok || got.Reachable != tc.reach || !strings.Contains(got.Message, tc.contains) {
				t.Errorf("Check = %+v", got)
			}
			if c := (*calls)[0]; c.method != http.MethodGet || c.path != "/users/appts@contoso.test/calendar" {
				t.Errorf("call = %s %s; Check must only read", c.method, c.path)
			}
		})
	}
}
