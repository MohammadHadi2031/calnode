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
