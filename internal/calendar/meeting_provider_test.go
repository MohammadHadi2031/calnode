package calendar

import (
	"context"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/slots"
)

type namedProvider struct {
	Provider
	name string
}

func (p namedProvider) Name() string { return p.name }
func (p namedProvider) FreeBusy(context.Context, string, time.Time, time.Time) ([]slots.Interval, error) {
	return nil, nil
}

// MeetingProvider looks past the destination: a host writing bookings to Outlook who
// also connected Google can still mint Meet, and Teams needs a work/school account.
func TestMeetingProvider(t *testing.T) {
	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn := func(userID, provider, kind string, dest int) {
		if _, err := database.ExecContext(ctx,
			`INSERT OR IGNORE INTO users (id, email, name, iana_timezone) VALUES (?, ?, 'U', 'UTC')`, userID, userID+"@x.test"); err != nil {
			t.Fatal(err)
		}
		if _, err := database.ExecContext(ctx,
			`INSERT INTO calendar_connections
			   (id, user_id, provider, access_token_enc, calendar_id, check_conflicts, is_destination, created_at, account_kind, account_email)
			 VALUES (?, ?, ?, 'e', 'primary', 1, ?, '2026-01-01T00:00:00Z', ?, ?)`,
			userID+"-"+provider+kind, userID, provider, dest, kind, userID+"-"+provider+"@acct.test"); err != nil {
			t.Fatal(err)
		}
	}
	conn("both", "microsoft", "work", 1) // destination Outlook...
	conn("both", "google", "", 0)        // ...plus a Google account
	conn("personal", "microsoft", "personal", 1)
	conn("gonly", "google", "", 1)

	svc := NewService(database)
	svc.Register(namedProvider{name: "google"})
	svc.Register(namedProvider{name: "microsoft"})
	for _, tc := range []struct{ user, loc, want string }{
		{"both", "google_meet", "google"},
		{"both", "teams", "microsoft"},
		{"personal", "teams", ""},
		{"gonly", "teams", ""},
		{"gonly", "google_meet", "google"},
		{"nobody", "google_meet", ""},
		{"both", "zoom", ""},
	} {
		got, err := svc.MeetingProvider(ctx, tc.user, tc.loc)
		if err != nil {
			t.Fatalf("MeetingProvider(%s, %s): %v", tc.user, tc.loc, err)
		}
		if got != tc.want {
			t.Errorf("MeetingProvider(%s, %s) = %q; want %q", tc.user, tc.loc, got, tc.want)
		}
	}
}

// fakeWorkspace is a configured (or not) workspace Teams organizer.
type fakeWorkspace struct {
	namedProvider
	mailbox string
	updated []string // calendarID:eventID per UpdateEvent call
}

func (f *fakeWorkspace) Mailbox(context.Context) (string, error) { return f.mailbox, nil }
func (f *fakeWorkspace) InvitesGuests() bool                     { return true }
func (f *fakeWorkspace) UpdateEvent(_ context.Context, _, calendarID, eventID string, _, _ time.Time) error {
	f.updated = append(f.updated, calendarID+":"+eventID)
	return nil
}

// A host without a Microsoft work account of their own gets Teams through the workspace
// organizer; their own connection to the workspace mailbox does not count as their own.
func TestMeetingProvider_workspaceFallback(t *testing.T) {
	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn := func(userID, provider, email string, dest int) {
		if _, err := database.ExecContext(ctx,
			`INSERT OR IGNORE INTO users (id, email, name, iana_timezone) VALUES (?, ?, 'U', 'UTC')`, userID, userID+"@x.test"); err != nil {
			t.Fatal(err)
		}
		if _, err := database.ExecContext(ctx,
			`INSERT INTO calendar_connections
			   (id, user_id, provider, access_token_enc, calendar_id, check_conflicts, is_destination, created_at, account_kind, account_email)
			 VALUES (?, ?, ?, 'e', 'primary', 1, ?, '2026-01-01T00:00:00Z', 'work', ?)`,
			userID+"-"+email, userID, provider, dest, email); err != nil {
			t.Fatal(err)
		}
	}
	conn("gonly", "google", "g@gmail.test", 1)
	conn("shared", "google", "s@gmail.test", 1)
	conn("shared", "microsoft", "Appts@Contoso.test", 0) // the old per-host way of sharing it
	conn("own", "google", "o@gmail.test", 1)
	conn("own", "microsoft", "own@contoso.test", 0)

	ws := &fakeWorkspace{namedProvider: namedProvider{name: "microsoft_workspace"}, mailbox: "appts@contoso.test"}
	svc := NewService(database)
	svc.Register(namedProvider{name: "google"})
	svc.Register(namedProvider{name: "microsoft"})
	svc.SetWorkspace(ws)

	for _, tc := range []struct{ user, loc, want string }{
		{"gonly", "teams", "microsoft_workspace"},
		{"shared", "teams", "microsoft_workspace"},
		{"own", "teams", "microsoft"}, // their own licensed account wins
		{"nobody", "teams", "microsoft_workspace"},
		{"gonly", "google_meet", "google"}, // only Teams falls back
		{"nobody", "google_meet", ""},
	} {
		got, err := svc.MeetingProvider(ctx, tc.user, tc.loc)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("MeetingProvider(%s, %s) = %q; want %q", tc.user, tc.loc, got, tc.want)
		}
	}
	for _, user := range []string{"gonly", "nobody"} {
		if ok, _ := svc.CanAutoGenerate(ctx, user, "teams"); !ok {
			t.Errorf("CanAutoGenerate(%s, teams) = false with a workspace mailbox", user)
		}
	}

	// Events it wrote route back to it by their stamped provider.
	if err := svc.UpdateEvent(ctx, "gonly", "appts@contoso.test", "ev1", "microsoft_workspace", time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(ws.updated) != 1 || ws.updated[0] != "appts@contoso.test:ev1" {
		t.Errorf("workspace updates = %v", ws.updated)
	}
	if !svc.ProviderInvitesGuests("microsoft_workspace") {
		t.Error("ProviderInvitesGuests(workspace) = false; its name must resolve")
	}

	// Unset mailbox: no fallback, and the per-host shared connection counts again.
	ws.mailbox = ""
	for _, tc := range []struct{ user, want string }{{"gonly", ""}, {"shared", "microsoft"}} {
		if got, _ := svc.MeetingProvider(ctx, tc.user, "teams"); got != tc.want {
			t.Errorf("unconfigured: MeetingProvider(%s) = %q; want %q", tc.user, got, tc.want)
		}
	}
	if names := svc.ProviderNames(); len(names) != 2 {
		t.Errorf("ProviderNames = %v; the workspace must never be a connect option", names)
	}
}
