-- +goose Up
-- Fork migration (wisegrid): a per-booking log of every booking email attempt, so staff
-- can see whether the booker, the guests and the hosts actually got their email, resend
-- it, and be alerted when one is not delivered. See docs/fork-email-log.md.
-- (90002 is the staff-calendar branch's; numbered apart so the branches merge cleanly.)
--
-- One row per attempt, not per email: a retried send leaves its failed attempt behind,
-- which is the evidence of what went wrong.
CREATE TABLE IF NOT EXISTS email_log (
    id          TEXT PRIMARY KEY,
    booking_id  TEXT NOT NULL REFERENCES bookings(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL,  -- confirmation | cancellation | reschedule | reminder
    role        TEXT NOT NULL,  -- attendee | guest | host
    recipient   TEXT NOT NULL,
    subject     TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'queued'
                  CHECK (status IN ('queued', 'sent', 'failed', 'bounced')),
    error       TEXT NOT NULL DEFAULT '',
    -- The transport's id for the message (Resend only), which a bounce refers to.
    provider_id TEXT NOT NULL DEFAULT '',
    -- When an alert went out for this failure; NULL = none (yet).
    alerted_at  TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS ix_email_log_booking ON email_log (booking_id, created_at);
CREATE INDEX IF NOT EXISTS ix_email_log_provider ON email_log (provider_id) WHERE provider_id != '';

-- +goose Down
DROP INDEX IF EXISTS ix_email_log_provider;
DROP INDEX IF EXISTS ix_email_log_booking;
DROP TABLE IF EXISTS email_log;
