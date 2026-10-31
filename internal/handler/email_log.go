package handler

// Booking email log, resend and failure alerts (wisegrid fork). See docs/fork-email-log.md.
//
// Every booking email (confirmation, cancellation, reschedule, reminder; to the booker, a
// guest or a host) goes through the Live mailer with a mailer.LogTag. emailLogObserver
// records each attempt in email_log; a failed or bounced one queues an email.alert job,
// which waits out the send's own retry and then tells the owner and the host.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/calnode/calnode/internal/booking"
	"github.com/calnode/calnode/internal/mailer"
	"github.com/calnode/calnode/internal/uid"
	"github.com/calnode/calnode/internal/webhook"
)

const (
	// emailAlertDelay is how long an alert waits after a failed attempt. sendWithRetry
	// retries a confirmation after 5s and the worker retries a reminder within a minute;
	// alerting only if the latest attempt is still undelivered after this avoids paging
	// anyone over a blip the retry already fixed.
	emailAlertDelay = 2 * time.Minute
	// emailAlertQuiet suppresses repeat alerts for the same email to the same recipient.
	emailAlertQuiet = 6 * time.Hour
	// maxEmailError bounds the stored error text (an SMTP transcript can be long).
	maxEmailError = 500
)

func nowNano() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// EmailLogObserver returns the observer server wiring installs on the Live mailer.
func (h *Handler) EmailLogObserver() mailer.SendObserver { return emailLogObserver{h: h} }

type emailLogObserver struct{ h *Handler }

// Begin records the attempt as queued and returns the func that records its outcome.
// Logging never affects the send: a row that cannot be written is logged and skipped.
func (o emailLogObserver) Begin(ctx context.Context, tag mailer.LogTag, msg mailer.Message) func(string, error) {
	h := o.h
	// Not bound to the send's context: the outcome must be recorded even when the send
	// failed because that context ran out.
	bg := context.WithoutCancel(ctx)
	id, now := uid.New(), nowNano()
	recipient := strings.Join(msg.To, ", ")
	if _, err := h.db.ExecContext(bg, `
		INSERT INTO email_log (id, booking_id, kind, role, recipient, subject, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'queued', ?, ?)`,
		id, tag.BookingID, tag.Kind, tag.Role, recipient, msg.Subject, now, now); err != nil {
		h.logger.Warn("email log: record attempt", "error", err, "booking_id", tag.BookingID, "kind", tag.Kind)
		return func(string, error) {}
	}
	return func(providerID string, sendErr error) {
		status, errText := "sent", ""
		if sendErr != nil {
			status, errText = "failed", truncateRunes(sendErr.Error(), maxEmailError)
		}
		if _, err := h.db.ExecContext(bg, `
			UPDATE email_log SET status = ?, error = ?, provider_id = ?, updated_at = ? WHERE id = ?`,
			status, errText, providerID, nowNano(), id); err != nil {
			h.logger.Warn("email log: record outcome", "error", err, "booking_id", tag.BookingID, "kind", tag.Kind)
		}
		if sendErr != nil {
			h.enqueueEmailAlert(bg, tag.BookingID, tag.Kind, recipient)
		}
	}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// enqueueEmailAlert queues the delayed alert for one undelivered email. The (type, payload)
// uniqueness on live jobs folds repeated failures of the same email into one pending alert.
func (h *Handler) enqueueEmailAlert(ctx context.Context, bookingID, kind, recipient string) {
	payload, _ := json.Marshal(map[string]string{
		"booking_id": bookingID, "kind": kind, "recipient": strings.ToLower(recipient),
	})
	runAt := time.Now().UTC().Add(emailAlertDelay).Format(time.RFC3339)
	if _, err := h.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO jobs (id, type, payload, run_at, status, attempts, max_attempts)
		VALUES (?, 'email.alert', ?, ?, 'pending', 0, 3)`,
		uid.New(), string(payload), runAt); err != nil {
		h.logger.Error("email log: queue alert", "error", err, "booking_id", bookingID, "kind", kind)
	}
}

// JobEmailAlert is the worker handler for email.alert jobs. If the latest attempt at the
// email is still failed or bounced, it fires booking.email_failed to the host's and the
// owner's webhooks and emails the owner, once per emailAlertQuiet.
func (h *Handler) JobEmailAlert(ctx context.Context, payload string) error {
	var p struct {
		BookingID string `json:"booking_id"`
		Kind      string `json:"kind"`
		Recipient string `json:"recipient"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return fmt.Errorf("email alert: parse payload: %w", err)
	}
	var logID, status, errText, role string
	err := h.db.QueryRowContext(ctx, `
		SELECT id, status, error, role FROM email_log
		WHERE booking_id = ? AND kind = ? AND recipient = ? COLLATE NOCASE
		ORDER BY created_at DESC LIMIT 1`, p.BookingID, p.Kind, p.Recipient).
		Scan(&logID, &status, &errText, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // booking deleted since
	}
	if err != nil {
		return fmt.Errorf("email alert: load attempt: %w", err)
	}
	if status != "failed" && status != "bounced" {
		return nil // a retry or a resend got it through
	}
	since := time.Now().UTC().Add(-emailAlertQuiet).Format(time.RFC3339Nano)
	var recent int
	if err := h.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM email_log
		WHERE booking_id = ? AND kind = ? AND recipient = ? COLLATE NOCASE AND alerted_at > ?`,
		p.BookingID, p.Kind, p.Recipient, since).Scan(&recent); err != nil {
		return fmt.Errorf("email alert: check recent: %w", err)
	}
	if recent > 0 {
		return nil
	}
	if _, err := h.db.ExecContext(ctx,
		`UPDATE email_log SET alerted_at = ? WHERE id = ?`, nowNano(), logID); err != nil {
		return fmt.Errorf("email alert: mark: %w", err)
	}

	b, err := h.bookingSvc.Get(ctx, p.BookingID)
	if err != nil {
		if errors.Is(err, booking.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("email alert: load booking: %w", err)
	}
	var slug, etName string
	_ = h.db.QueryRowContext(ctx, `SELECT slug, name FROM event_types WHERE id = ?`, b.EventTypeID).Scan(&slug, &etName)

	owners, err := h.alertOwners(ctx)
	if err != nil {
		h.logger.Error("email alert: load owners", "error", err, "booking_id", b.ID)
	}

	if h.webhookSvc != nil {
		wp := webhook.BookingPayload{
			ID:             b.ID,
			EventTypeSlug:  slug,
			HostID:         b.HostID,
			StartAt:        b.StartAt.UTC().Format(time.RFC3339),
			EndAt:          b.EndAt.UTC().Format(time.RFC3339),
			Status:         b.Status,
			LocationValue:  b.LocationValue,
			CreatedAt:      b.CreatedAt.UTC().Format(time.RFC3339),
			EmailKind:      p.Kind,
			EmailStatus:    status,
			EmailError:     errText,
			EmailRecipient: p.Recipient,
		}
		if err := h.webhookSvc.Enqueue(ctx, "booking.email_failed", wp); err != nil {
			h.logger.Error("email alert: enqueue host webhook", "error", err, "booking_id", b.ID)
		}
		for _, o := range owners {
			if o.ID == b.HostID {
				continue // already covered by the host's own webhooks
			}
			if err := h.webhookSvc.EnqueueFor(ctx, o.ID, "booking.email_failed", wp); err != nil {
				h.logger.Error("email alert: enqueue owner webhook", "error", err, "booking_id", b.ID)
			}
		}
	}

	// The alert email goes through the same transport that may have just failed, so it is
	// best effort: the webhook is the channel that survives broken SMTP credentials. Sent
	// untagged, so it is not itself logged or alerted on.
	subject := fmt.Sprintf("Email not delivered: %s for %s (%s)", p.Kind, etName, b.StartAt.UTC().Format("Mon 2 Jan 15:04 UTC"))
	body := fmt.Sprintf(`A booking email was not delivered.

Booking:   %s
Event:     %s
Starts:    %s
Email:     %s to the %s
Recipient: %s
Status:    %s
Error:     %s

Open the booking in %s/admin/bookings to see the email log and resend it.
`, b.ID, etName, b.StartAt.UTC().Format(time.RFC1123), p.Kind, role, p.Recipient, status, errText,
		strings.TrimRight(h.baseURL, "/"))
	for _, o := range owners {
		if o.Email == "" || strings.Contains(p.Recipient, strings.ToLower(o.Email)) {
			continue // never alert an address about its own undeliverable mail
		}
		if err := h.mailer.Send(ctx, mailer.Message{To: []string{o.Email}, Subject: subject, Text: body}); err != nil {
			h.logger.Warn("email alert: email owner", "error", err, "booking_id", b.ID)
		}
	}
	h.logger.Warn("booking email not delivered", "booking_id", b.ID, "kind", p.Kind, "role", role, "status", status, "error", errText)
	return nil
}

type alertOwner struct{ ID, Email string }

// alertOwners returns the active workspace owners, who receive every email alert.
func (h *Handler) alertOwners(ctx context.Context) ([]alertOwner, error) {
	rows, err := h.db.QueryContext(ctx,
		`SELECT id, email FROM users WHERE is_owner = 1 AND archived_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []alertOwner
	for rows.Next() {
		var o alertOwner
		if err := rows.Scan(&o.ID, &o.Email); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// recordEmailBounce marks the attempt Resend reported as bounced and queues its alert.
// A bounced booker confirmation also flags the booking, like a failed send does.
func (h *Handler) recordEmailBounce(ctx context.Context, providerID, reason string) (bool, error) {
	var logID, bookingID, kind, role, recipient string
	err := h.db.QueryRowContext(ctx, `
		SELECT id, booking_id, kind, role, recipient FROM email_log WHERE provider_id = ?`, providerID).
		Scan(&logID, &bookingID, &kind, &role, &recipient)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // not a booking email (or older than the log)
	}
	if err != nil {
		return false, err
	}
	if reason == "" {
		reason = "bounced"
	}
	if _, err := h.db.ExecContext(ctx, `
		UPDATE email_log SET status = 'bounced', error = ?, updated_at = ? WHERE id = ?`,
		truncateRunes(reason, maxEmailError), nowNano(), logID); err != nil {
		return false, err
	}
	if kind == "confirmation" && role == "attendee" {
		if _, err := h.db.ExecContext(ctx,
			`UPDATE bookings SET confirm_failed = 1 WHERE id = ?`, bookingID); err != nil {
			h.logger.Error("email bounce: flag booking", "error", err, "booking_id", bookingID)
		}
	}
	h.enqueueEmailAlert(ctx, bookingID, kind, recipient)
	return true, nil
}

// canManageBooking reports whether user may act on a booking's emails and guests (email
// log, resend, adding guests): admins for every booking, a host for any booking they hold
// a seat on (the bookings list's visibility rule, ARCHITECTURE §14), and the secretary of
// the booking's primary host (the Schedule page's rule, docs/fork-staff-calendar.md).
// Writes the 404/500 itself.
func (h *Handler) canManageBooking(w http.ResponseWriter, r *http.Request, user AuthUser, bookingID string) bool {
	var n int
	err := h.db.QueryRowContext(r.Context(), `
		SELECT COUNT(*) FROM bookings b
		WHERE b.id = ? AND (? OR b.host_id = ?
		  OR EXISTS (SELECT 1 FROM booking_hosts bh WHERE bh.booking_id = b.id AND bh.user_id = ?)
		  OR EXISTS (SELECT 1 FROM secretary_hosts sh WHERE sh.secretary_id = ? AND sh.host_id = b.host_id))`,
		bookingID, user.IsAdmin, user.ID, user.ID, user.ID).Scan(&n)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "booking emails: authorize", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return false
	}
	if n == 0 {
		h.writeError(w, http.StatusNotFound, "booking not found")
		return false
	}
	return true
}

type emailLogJSON struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Role      string `json:"role"`
	Recipient string `json:"recipient"`
	Subject   string `json:"subject"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ListBookingEmails handles GET /v1/bookings/{id}/emails: every logged email attempt for
// the booking, newest first.
func (h *Handler) ListBookingEmails(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	id := r.PathValue("id")
	if !h.canManageBooking(w, r, user, id) {
		return
	}
	rows, err := h.db.QueryContext(r.Context(), `
		SELECT id, kind, role, recipient, subject, status, error, created_at, updated_at
		FROM email_log WHERE booking_id = ? ORDER BY created_at DESC LIMIT 200`, id)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "booking emails: query", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	items := []emailLogJSON{}
	for rows.Next() {
		var e emailLogJSON
		if err := rows.Scan(&e.ID, &e.Kind, &e.Role, &e.Recipient, &e.Subject, &e.Status, &e.Error,
			&e.CreatedAt, &e.UpdatedAt); err != nil {
			h.logger.ErrorContext(r.Context(), "booking emails: scan", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		items = append(items, e)
	}
	if err := rows.Err(); err != nil {
		h.logger.ErrorContext(r.Context(), "booking emails: rows", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// resendBudget bounds a resend, which runs inside the request so staff see the outcome.
var resendBudget = 60 * time.Second

// ResendConfirmation handles POST /v1/bookings/{id}/resend-confirmation. Body (optional):
// {"to": "attendee" | "hosts"}. "attendee" (the default) re-sends the booker's
// confirmation, with a fresh manage link, plus each guest's copy where Calnode is the one
// inviting guests; "hosts" re-sends every assigned host's new-booking notification. One
// attempt each, no background retry: the result is in the email log the caller reloads.
func (h *Handler) ResendConfirmation(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req struct {
		To string `json:"to"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // empty body = defaults
	if req.To == "" {
		req.To = "attendee"
	}
	if req.To != "attendee" && req.To != "hosts" {
		h.writeError(w, http.StatusBadRequest, `"to" must be "attendee" or "hosts"`)
		return
	}
	if !h.canManageBooking(w, r, user, id) {
		return
	}
	b, err := h.bookingSvc.Get(r.Context(), id)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "resend confirmation: load booking", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if b.Status != "confirmed" {
		h.writeError(w, http.StatusConflict, "only a confirmed booking can be re-confirmed")
		return
	}
	if !h.isEmailEnabled() {
		h.writeError(w, http.StatusConflict, "email is not configured (Settings → Email)")
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), resendBudget)
	defer cancel()
	d, err := h.loadConfirmationData(ctx, b, true)
	if err != nil {
		h.logger.ErrorContext(ctx, "resend confirmation: load data", "error", err, "booking_id", b.ID)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	hosts, err := h.assignedHosts(ctx, b.ID)
	if err != nil || len(hosts) == 0 {
		hosts = []assignedHost{{UserID: b.HostID, Name: d.HostName, Email: d.HostEmail, IsPrimary: true}}
	}
	// Whether the hosts' calendars invited the booker and guests (calendar mode with an
	// event written). Approximates the original send, which knew this from the events it
	// had just created: a provider-invited booker must not get a second, differently
	// UID'd .ics, and its guests were invited by the provider rather than by us.
	providerInvites := false
	if b.InviteDelivery != booking.InviteByCalnode {
		for _, host := range hosts {
			if host.ExternalEventID != "" || host.InvitedViaOrganizer {
				providerInvites = true
				break
			}
		}
	}

	sent, failed := 0, 0
	count := func(err error) {
		if err != nil {
			failed++
		} else {
			sent++
		}
	}
	if req.To == "hosts" {
		for _, host := range hosts {
			hd := h.hostBookingData(ctx, d, host, b.InviteDelivery, b.UpdatedAt)
			if providerInvites {
				hd.AttachICS = false
			}
			count(mailer.SendConfirmationToHost(ctx, h.mailer, hd))
		}
	} else {
		primary := primaryHost(hosts)
		d.HostName, d.HostEmail = primary.Name, primary.Email
		h.applyInviteDelivery(ctx, &d, b.InviteDelivery, b.HostID)
		if b.InviteDelivery != booking.InviteByCalnode {
			d.AttachICS = !providerInvites
		}
		d.ICSSequence = int(b.UpdatedAt.Unix())
		err := mailer.SendConfirmationToAttendee(ctx, h.mailer, d)
		count(err)
		if err == nil {
			if _, err := h.db.ExecContext(ctx,
				`UPDATE bookings SET confirm_failed = 0 WHERE id = ?`, b.ID); err != nil {
				h.logger.ErrorContext(ctx, "resend confirmation: clear flag", "error", err, "booking_id", b.ID)
			}
		}
		if !providerInvites {
			for _, g := range d.Guests {
				count(mailer.SendConfirmationToAttendee(ctx, h.mailer, d.ForGuest(g)))
			}
		}
	}
	h.writeJSON(w, http.StatusOK, map[string]int{"sent": sent, "failed": failed})
}

// loadConfirmationData rebuilds a booking's confirmation email data from the database,
// for a resend: what dispatchBookingConfirmation had in hand at booking time, minus the
// calendar phase (the stored location already holds any minted link). withManageLink
// issues a fresh manage link (only a hash of the original is stored); a guest's copy has
// none, so it needs none.
func (h *Handler) loadConfirmationData(ctx context.Context, b *booking.Booking, withManageLink bool) (mailer.BookingData, error) {
	d, err := h.loadCancellationData(ctx, b)
	if err != nil {
		return d, err
	}
	d.CancellationReason = ""
	d.BaseURL = h.publicURL()
	h.applyBranding(ctx, &d)
	var msgNote, subjNote sql.NullString
	_ = h.db.QueryRowContext(ctx, `SELECT msg_confirmation, subj_confirmation FROM event_types WHERE id = ?`, b.EventTypeID).
		Scan(&msgNote, &subjNote)
	d.CustomNote, d.SubjectOverride = msgNote.String, subjNote.String
	if withManageLink {
		if tok, err := h.bookingSvc.IssueManageToken(ctx, b.ID); err != nil {
			h.logger.ErrorContext(ctx, "resend confirmation: issue manage token", "error", err, "booking_id", b.ID)
		} else {
			d.ManageURL = h.publicURL() + "/manage/" + tok
		}
	}
	_, d.Guests = guestInvitees(h.loadGuests(ctx, b.ID))
	return d, nil
}
