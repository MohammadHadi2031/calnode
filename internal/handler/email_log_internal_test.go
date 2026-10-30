package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/mailer"
	"github.com/calnode/calnode/internal/webhook"
)

// flakyMailer is a transport that can be broken on purpose, like SMTP with wrong
// credentials. It reports a Resend-style message id for each delivered message.
type flakyMailer struct {
	mu   sync.Mutex
	sent []mailer.Message
	fail atomic.Bool
	n    atomic.Int64
}

func (m *flakyMailer) Send(ctx context.Context, msg mailer.Message) error {
	_, err := m.SendID(ctx, msg)
	return err
}

func (m *flakyMailer) SendID(_ context.Context, msg mailer.Message) (string, error) {
	if m.fail.Load() {
		return "", errors.New("535 5.7.8 authentication failed")
	}
	m.mu.Lock()
	m.sent = append(m.sent, msg)
	m.mu.Unlock()
	return fmt.Sprintf("re_%d", m.n.Add(1)), nil
}

type emailLogFixture struct {
	*inviteFixture
	flaky  *flakyMailer
	userID string
	whs    *webhook.Service
}

// newEmailLogFixture is the invite fixture wired like production: every send goes
// through a Live mailer with the email log observer installed.
func newEmailLogFixture(t *testing.T) *emailLogFixture {
	t.Helper()
	f := &emailLogFixture{inviteFixture: newRSVPFixture(t), flaky: &flakyMailer{}}
	live := mailer.NewLive(f.flaky)
	live.SetObserver(f.h.EmailLogObserver())
	f.h.SetMailer(live, "https://book.team.example")
	whs, err := webhook.New(f.h.db, "")
	if err != nil {
		t.Fatal(err)
	}
	f.whs = whs
	f.h.SetWebhookSvc(whs)
	if err := f.h.db.QueryRow(`SELECT id FROM users WHERE email = ?`, inviteHostEmail).Scan(&f.userID); err != nil {
		t.Fatalf("load host: %v", err)
	}
	if rec := f.createEventType(t, "intro", "calnode"); rec.Code != http.StatusCreated {
		t.Fatalf("create event type: %d — %s", rec.Code, rec.Body.String())
	}
	return f
}

// createBooking books through the public endpoint without waiting for any email.
func (f *emailLogFixture) createBooking(t *testing.T) string {
	t.Helper()
	start := time.Now().UTC().Add(72 * time.Hour).Truncate(24 * time.Hour).Add(10 * time.Hour)
	req := httptest.NewRequest(http.MethodPost, "/v1/bookings", strings.NewReader(fmt.Sprintf(
		`{"event_type_slug":"intro","start_at":%q,"name":"Guest","email":%q,"timezone":"UTC"}`,
		start.Format(time.RFC3339), inviteBookerEmail)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.h.CreateBooking(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create booking: %d — %s", rec.Code, rec.Body.String())
	}
	var b struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	return b.ID
}

type logRow struct{ Kind, Role, Recipient, Status, Error, ProviderID string }

func (f *emailLogFixture) logRows(t *testing.T, bookingID string) []logRow {
	t.Helper()
	rows, err := f.h.db.Query(`SELECT kind, role, recipient, status, error, provider_id
		FROM email_log WHERE booking_id = ? ORDER BY created_at`, bookingID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []logRow
	for rows.Next() {
		var r logRow
		if err := rows.Scan(&r.Kind, &r.Role, &r.Recipient, &r.Status, &r.Error, &r.ProviderID); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// waitLog polls until cond holds for the booking's log rows (sends run in the background).
func (f *emailLogFixture) waitLog(t *testing.T, bookingID string, within time.Duration, cond func([]logRow) bool) []logRow {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		rows := f.logRows(t, bookingID)
		if cond(rows) {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("email log never reached the expected state; rows: %+v", rows)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func hasRow(rows []logRow, role, status string) bool {
	for _, r := range rows {
		if r.Role == role && r.Status == status {
			return true
		}
	}
	return false
}

func (f *emailLogFixture) confirmFailed(t *testing.T, bookingID string) bool {
	t.Helper()
	var v int
	if err := f.h.db.QueryRow(`SELECT confirm_failed FROM bookings WHERE id = ?`, bookingID).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v == 1
}

func (f *emailLogFixture) bookingReq(t *testing.T, fn http.HandlerFunc, method, bookingID, suffix, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/v1/bookings/"+bookingID+suffix, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", f.apiKey)
	req.SetPathValue("id", bookingID)
	rec := httptest.NewRecorder()
	f.h.RequireAuth(fn)(rec, req)
	return rec
}

// A booking's confirmation emails are logged per recipient with the provider's id.
func TestEmailLog_recordsConfirmations(t *testing.T) {
	f := newEmailLogFixture(t)
	id := f.createBooking(t)
	rows := f.waitLog(t, id, 3*time.Second, func(rows []logRow) bool {
		return hasRow(rows, "attendee", "sent") && hasRow(rows, "host", "sent")
	})
	for _, r := range rows {
		if r.Kind != "confirmation" || r.ProviderID == "" {
			t.Errorf("row %+v: want a confirmation carrying the provider id", r)
		}
		if r.Role == "attendee" && r.Recipient != inviteBookerEmail {
			t.Errorf("attendee row went to %q", r.Recipient)
		}
	}

	rec := f.bookingReq(t, f.h.ListBookingEmails, http.MethodGet, id, "/emails", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list emails: %d — %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Items []emailLogJSON `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Items) != len(rows) {
		t.Fatalf("list emails = %s; want %d items", rec.Body.String(), len(rows))
	}

	if rec := f.bookingReq(t, f.h.ListBookingEmails, http.MethodGet, "no-such-booking", "/emails", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown booking: %d; want 404", rec.Code)
	}
}

// R3 acceptance: break the transport on purpose. The booking still succeeds, the email
// shows as failed, an alert goes out once the retry has also failed, and resend works
// after the fix.
func TestEmailLog_brokenTransportAlertsAndResendRecovers(t *testing.T) {
	f := newEmailLogFixture(t)
	if _, _, err := f.whs.Create(context.Background(), f.userID, "https://hooks.team.example/alert",
		[]string{"booking.email_failed"}); err != nil {
		t.Fatal(err)
	}
	f.flaky.fail.Store(true)

	id := f.createBooking(t) // fatal unless 201
	// sendWithRetry waits 5s before its second attempt.
	f.waitLog(t, id, 10*time.Second, func(rows []logRow) bool {
		n := 0
		for _, r := range rows {
			if r.Role == "attendee" && r.Status == "failed" {
				n++
			}
		}
		return n == 2
	})
	deadline := time.Now().Add(2 * time.Second)
	for !f.confirmFailed(t, id) {
		if time.Now().After(deadline) {
			t.Fatal("booking was not flagged confirm_failed")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// One pending alert per undelivered email, however many attempts failed.
	var payload string
	if err := f.h.db.QueryRow(`SELECT payload FROM jobs WHERE type = 'email.alert' AND status = 'pending'
		AND payload LIKE ?`, "%"+inviteBookerEmail+"%").Scan(&payload); err != nil {
		t.Fatalf("alert job for the booker: %v", err)
	}
	if err := f.h.JobEmailAlert(context.Background(), payload); err != nil {
		t.Fatalf("run alert: %v", err)
	}
	var deliveries int
	_ = f.h.db.QueryRow(`SELECT COUNT(*) FROM webhook_deliveries WHERE event = 'booking.email_failed' AND booking_id = ?`, id).Scan(&deliveries)
	if deliveries != 1 {
		t.Fatalf("booking.email_failed deliveries = %d; want 1", deliveries)
	}
	var dp string
	_ = f.h.db.QueryRow(`SELECT payload FROM webhook_deliveries WHERE booking_id = ?`, id).Scan(&dp)
	if !strings.Contains(dp, `"email_kind":"confirmation"`) || !strings.Contains(dp, `"email_status":"failed"`) ||
		!strings.Contains(dp, "authentication failed") {
		t.Errorf("webhook payload = %s; want the kind, status and error", dp)
	}
	// Running it again inside the quiet period does not alert twice.
	if err := f.h.JobEmailAlert(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	_ = f.h.db.QueryRow(`SELECT COUNT(*) FROM webhook_deliveries WHERE booking_id = ?`, id).Scan(&deliveries)
	if deliveries != 1 {
		t.Errorf("deliveries after a repeat run = %d; want still 1", deliveries)
	}

	// Fix the transport and resend.
	f.flaky.fail.Store(false)
	rec := f.bookingReq(t, f.h.ResendConfirmation, http.MethodPost, id, "/resend-confirmation", `{}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"failed":0`) {
		t.Fatalf("resend: %d — %s", rec.Code, rec.Body.String())
	}
	if f.confirmFailed(t, id) {
		t.Error("confirm_failed still set after a successful resend")
	}
	rows := f.logRows(t, id)
	if last := rows[len(rows)-1]; last.Role != "attendee" || last.Status != "sent" {
		t.Errorf("latest attempt = %+v; want the booker's confirmation, sent", last)
	}
	f.flaky.mu.Lock()
	var resent *mailer.Message
	for i := range f.flaky.sent {
		if f.flaky.sent[i].To[0] == inviteBookerEmail {
			resent = &f.flaky.sent[i]
		}
	}
	f.flaky.mu.Unlock()
	if resent == nil || !strings.Contains(resent.Text, "/manage/") || icsOf(resent) == "" {
		t.Errorf("resent confirmation must carry a manage link and the invite; got %+v", resent)
	}

	// The hosts' notifications can be resent too.
	if rec := f.bookingReq(t, f.h.ResendConfirmation, http.MethodPost, id, "/resend-confirmation", `{"to":"hosts"}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"sent":1`) {
		t.Errorf("resend to hosts: %d — %s", rec.Code, rec.Body.String())
	}
	if rec := f.bookingReq(t, f.h.ResendConfirmation, http.MethodPost, id, "/resend-confirmation", `{"to":"everyone"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad target: %d; want 400", rec.Code)
	}
}

// With no transport configured nothing is delivered: the log says so instead of "sent".
func TestEmailLog_noopMailerIsLoggedAsFailed(t *testing.T) {
	f := newEmailLogFixture(t)
	live := mailer.NewLive(&mailer.Noop{})
	live.SetObserver(f.h.EmailLogObserver())
	f.h.SetMailer(live, "https://book.team.example")
	id := f.createBooking(t)
	rows := f.waitLog(t, id, 3*time.Second, func(rows []logRow) bool { return hasRow(rows, "attendee", "failed") })
	for _, r := range rows {
		if r.Status == "sent" {
			t.Errorf("row %+v marked sent with no transport", r)
		}
	}
}

// signedResendEvent posts body to the Resend webhook endpoint, signed with the fixture's secret.
func (f *emailLogFixture) signedResendEvent(t *testing.T, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	key, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(rsvpSecret, "whsec_"))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("msg_b." + ts + "."))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/email/inbound/resend", strings.NewReader(string(body)))
	req.Header.Set("svix-id", "msg_b")
	req.Header.Set("svix-timestamp", ts)
	req.Header.Set("svix-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	f.h.InboundEmailResend(rec, req)
	return rec
}

// A bounce Resend reports later turns the logged attempt into "bounced" and flags the booking.
func TestEmailLog_resendBounce(t *testing.T) {
	f := newEmailLogFixture(t)
	id := f.createBooking(t)
	rows := f.waitLog(t, id, 3*time.Second, func(rows []logRow) bool { return hasRow(rows, "attendee", "sent") })
	var providerID string
	for _, r := range rows {
		if r.Role == "attendee" {
			providerID = r.ProviderID
		}
	}
	body, _ := json.Marshal(map[string]any{
		"type": "email.bounced",
		"data": map[string]any{
			"email_id": providerID,
			"to":       []string{inviteBookerEmail},
			"bounce":   map[string]any{"message": "550 5.1.1 mailbox does not exist", "type": "Permanent"},
		},
	})
	if rec := f.signedResendEvent(t, body); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "bounced") {
		t.Fatalf("bounce webhook: %d — %s", rec.Code, rec.Body.String())
	}
	rows = f.logRows(t, id)
	if !hasRow(rows, "attendee", "bounced") {
		t.Errorf("rows = %+v; want the booker's confirmation bounced", rows)
	}
	if !f.confirmFailed(t, id) {
		t.Error("a bounced confirmation must flag the booking")
	}
	var jobs int
	_ = f.h.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE type = 'email.alert' AND status = 'pending'`).Scan(&jobs)
	if jobs != 1 {
		t.Errorf("pending alerts = %d; want 1", jobs)
	}

	// A bounce for mail we never logged is acknowledged and ignored.
	body, _ = json.Marshal(map[string]any{"type": "email.bounced", "data": map[string]any{"email_id": "re_unknown"}})
	if rec := f.signedResendEvent(t, body); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ignored") {
		t.Errorf("unknown bounce: %d — %s", rec.Code, rec.Body.String())
	}
}
