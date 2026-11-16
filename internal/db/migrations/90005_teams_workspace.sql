-- +goose Up
-- Fork migration (wisegrid): the workspace Teams account. One Microsoft 365 mailbox,
-- set by an admin in Settings → Microsoft Teams, organizes the Teams meeting for every
-- host who has no Microsoft work account of their own. The app reaches it with its own
-- credentials (client credentials), so nobody signs in as it. '' = not configured.
-- See docs/fork-teams-setup.md.
ALTER TABLE server_settings ADD COLUMN teams_mailbox TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE server_settings DROP COLUMN teams_mailbox;
