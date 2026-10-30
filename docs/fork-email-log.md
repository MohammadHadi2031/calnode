# Booking email log, resend and failure alerts (wisegrid fork)

Every booking email is recorded per booking, staff can resend a confirmation, and an
email that is not delivered raises an alert. This covers requirement R3 in the booking
system checklist.

Fork migration: `90003_email_log.sql`.

## What is logged

The log records each attempt at these emails:

- confirmation
- cancellation
- reschedule
- reminder

It covers the booker (`attendee`), each guest (`guest`) and each host (`host`).

- Each attempt is one row in `email_log`, with the recipient, subject and status:
  - `queued` (sending now);
  - `sent` (the transport accepted it);
  - `failed` (the transport refused it, with the error);
  - `bounced` (accepted, then reported undeliverable).
- A retried send keeps its failed attempt as a separate row. The failed row is the record of what went wrong.
- Mail that is not about a booking is not logged: magic links, password resets and test emails.

How it works: the typed `mailer.Send*` helpers put a `mailer.LogTag` (booking, kind, role) on the
context. `mailer.Live` hands every tagged send to its `SendObserver`, which is
`emailLogObserver` in `internal/handler/email_log.go`. `Live` is not wrapped, because
callers type-assert it for `Swap` and `IsEnabled`.

With no transport configured (`Noop`), a booking email is logged as `failed` with "email is
not configured". The booking flow still treats it as success, as upstream does.

## Bounces

- **Resend:** the send stores Resend's message id. The instance's Resend webhook now also
  subscribes to `email.bounced`, and a bounce marks the matching row `bounced`. A bounced
  booker confirmation also sets `bookings.confirm_failed`.
  - The webhook is the one used for RSVP tracking (Settings → Email → set up the webhook).
    On an existing install, run that setup once more so the webhook adds `email.bounced`.
    It needs a full-access Resend key.
- **SMTP:** there is no bounce signal, because a bounce arrives later as mail to the
  sender's mailbox. Only refusals at send time (`failed`) are visible.

## Alerts

A failed or bounced attempt queues an `email.alert` job that runs 2 minutes later
(`emailAlertDelay`). This lets the send's own retry happen first: confirmations retry after
5 seconds, and reminders through the worker. When the job runs, if the latest attempt at
that email to that recipient is still failed or bounced, it does three things:

1. fires the `booking.email_failed` webhook to the booking host's webhooks and to every owner's webhooks;
2. emails each owner (best effort);
3. logs a warning.

There is at most one alert per email and recipient every 6 hours (`emailAlertQuiet`).

The alert email goes through the same transport that just failed, so with broken SMTP
credentials it cannot arrive. **Subscribe a webhook to `booking.email_failed`** (Settings →
Webhooks) to get alerts that do not depend on email.

Payload fields added for this event:

- in the default set: `email_kind`, `email_status`, `email_error`;
- opt-in, because it is PII: `email_recipient`.

## Admin UI and API

On the Bookings page, a confirmed booking whose booker confirmation failed or bounced shows
an **email failed** badge. Expanding the booking shows its **Emails** log with two buttons:

- **Resend confirmation**: the booker's confirmation, with a fresh manage link, plus guest
  copies when Calnode is the one inviting guests. On success it clears the badge.
- **Resend to hosts**: every assigned host's new-booking notification.

| Endpoint | Who |
| --- | --- |
| `GET /v1/bookings/{id}/emails` | admins, and any host with a seat on the booking |
| `POST /v1/bookings/{id}/resend-confirmation` with `{"to":"attendee"\|"hosts"}` | same |

A resend makes one attempt per recipient inside the request and returns `{sent, failed}`.
Its attempts appear in the log like any others.

The resend decides how to attach the calendar invite from what is stored. If the booking is
calendar-invited and a host event exists, the booker's provider invite already went out, so
no `.ics` is attached. This approximates the original send, which knew it from the events
it had just created.
