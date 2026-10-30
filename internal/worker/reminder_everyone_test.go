package worker_test

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/mailer"
	"github.com/calnode/calnode/internal/webhook"
	"github.com/calnode/calnode/internal/worker"
)

// seedGroupBooking is a confirmed booking hosted by host-01 (primary) and host-02, with
// one guest besides the booker, and its due reminder job.
func seedGroupBooking(t *testing.T, database *sql.DB) {
	t.Helper()
	ctx := context.Background()
	runAt := time.Now().UTC().Add(-time.Second).Format(time.RFC3339)
	start := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO event_types (id, user_id, slug, name, duration_minutes) VALUES ('et-g','host-01','demo','Product Demo',30)`, nil},
		{`INSERT INTO bookings (id, event_type_id, host_id, start_at, end_at, status, location_value)
		  VALUES ('bk-g','et-g','host-01',?,?,'confirmed','https://meet.google.com/abc-defg-hij')`, []any{start, start}},
		{`INSERT INTO booking_hosts (id, booking_id, user_id, is_primary) VALUES ('bh-1','bk-g','host-01',1),('bh-2','bk-g','host-02',0)`, nil},
		{`INSERT INTO booking_attendees (id, booking_id, name, email, iana_timezone, is_organizer)
		  VALUES ('a-1','bk-g','Lead','lead@client.example','UTC',1),('a-2','bk-g','Engineer','eng@client.example','UTC',0)`, nil},
		{`INSERT INTO jobs (id, type, payload, run_at, status, attempts, max_attempts)
		  VALUES ('job-g','reminder.send','{"booking_id":"bk-g"}',?,'pending',0,3)`, []any{runAt}},
	} {
		if _, err := database.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func runReminder(t *testing.T, database *sql.DB, svc *webhook.Service) map[string]mailer.Message {
	t.Helper()
	m := &captureMailer{}
	w := worker.New(database, svc, slog.Default(), worker.WithMailer(m),
		worker.WithHTTPClient(&http.Client{}), worker.WithPublicURL("https://book.team.example"))
	w.Poll(context.Background())
	byTo := map[string]mailer.Message{}
	for _, msg := range m.sent {
		byTo[msg.To[0]] = msg
	}
	return byTo
}

// Checklist R6: the reminder reaches every host and guest, with the conference link, and
// the booker's has a link to reschedule.
func TestWorker_reminderReachesEveryoneWithLinks(t *testing.T) {
	database, svc := setup(t)
	seedGroupBooking(t, database)
	sent := runReminder(t, database, svc)

	for _, to := range []string{"lead@client.example", "eng@client.example", "host01@example.com", "host02@example.com"} {
		msg, ok := sent[to]
		if !ok {
			t.Errorf("no reminder to %s; sent to %v", to, keys(sent))
			continue
		}
		if !strings.Contains(msg.Text, "https://meet.google.com/abc-defg-hij") {
			t.Errorf("reminder to %s lacks the conference link:\n%s", to, msg.Text)
		}
	}
	if len(sent) != 4 {
		t.Errorf("sent %d reminders; want 4 (booker, guest, two hosts)", len(sent))
	}
	if lead := sent["lead@client.example"]; !strings.Contains(lead.Text, "https://book.team.example/manage/") {
		t.Errorf("booker's reminder lacks the reschedule link:\n%s", lead.Text)
	}
	if eng := sent["eng@client.example"]; strings.Contains(eng.Text, "/manage/") || !strings.Contains(eng.Text, "Engineer") {
		t.Errorf("guest's reminder must greet them and carry no manage link:\n%s", eng.Text)
	}
	if host := sent["host02@example.com"]; !strings.Contains(host.Subject, "Reminder") ||
		!strings.Contains(host.Text, "Lead") || !strings.Contains(host.Text, "eng@client.example") {
		t.Errorf("host reminder should name the booker and the guests:\n%s\n%s", host.Subject, host.Text)
	}
}

// A host who turned their own reminder off gets none; turning off the attendee reminders
// (the primary host's "Reminder emails") no longer silences the hosts' reminders.
func TestWorker_reminderPreferences(t *testing.T) {
	database, svc := setup(t)
	seedGroupBooking(t, database)
	if _, err := database.Exec(`UPDATE users SET notify_host_reminder = 0 WHERE id = 'host-02'`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE users SET notify_reminder = 0 WHERE id = 'host-01'`); err != nil {
		t.Fatal(err)
	}
	sent := runReminder(t, database, svc)
	if len(sent) != 1 {
		t.Fatalf("sent to %v; want only host01@example.com", keys(sent))
	}
	if _, ok := sent["host01@example.com"]; !ok {
		t.Errorf("sent to %v; want host01@example.com", keys(sent))
	}
}

func keys(m map[string]mailer.Message) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
