-- +goose Up
-- Fork migration (wisegrid): reminders reach every host of a booking, not only the
-- booker. A host can turn their own copy off (Settings → Notifications, "Upcoming
-- meeting reminder"); on by default. See docs/fork-reminders.md.
ALTER TABLE users ADD COLUMN notify_host_reminder INTEGER NOT NULL DEFAULT 1;

-- +goose Down
ALTER TABLE users DROP COLUMN notify_host_reminder;
