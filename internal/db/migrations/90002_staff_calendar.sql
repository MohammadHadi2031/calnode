-- +goose Up
-- Fork (wisegrid): secretaries and unavailable blocks for the staff Schedule page.
-- See docs/fork-staff-calendar.md.

-- A secretary manages the calendars of the hosts assigned to them here. Any user with a
-- row is a secretary; admins already manage everyone and need no rows.
CREATE TABLE IF NOT EXISTS secretary_hosts (
    secretary_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    host_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (secretary_id, host_id),
    CHECK (secretary_id <> host_id)
);
CREATE INDEX IF NOT EXISTS ix_secretary_hosts_host ON secretary_hosts (host_id);

-- A time range a host cannot be booked, set from the Schedule page. Unlike
-- availability_overrides (whole dates, host-local), these are exact UTC instants.
-- With sync_to_calendar the block is also written to the host's destination calendar as
-- a busy event; external_* record where, so it can be moved and deleted.
CREATE TABLE IF NOT EXISTS unavailable_blocks (
    id                   TEXT PRIMARY KEY,
    user_id              TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    start_at             TEXT NOT NULL,   -- RFC3339 UTC
    end_at               TEXT NOT NULL,
    title                TEXT NOT NULL DEFAULT '',
    sync_to_calendar     INTEGER NOT NULL DEFAULT 1,
    external_event_id    TEXT NOT NULL DEFAULT '',
    external_calendar_id TEXT NOT NULL DEFAULT '',
    external_provider    TEXT NOT NULL DEFAULT '',
    created_by           TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at           TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at           TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    CHECK (end_at > start_at)
);
CREATE INDEX IF NOT EXISTS ix_unavailable_blocks_user_start ON unavailable_blocks (user_id, start_at);

-- +goose Down
DROP TABLE IF EXISTS unavailable_blocks;
DROP TABLE IF EXISTS secretary_hosts;
