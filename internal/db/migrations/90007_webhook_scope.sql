-- +goose Up
-- Fork migration (wisegrid): a webhook fires for its owner's bookings ('own', the
-- historical behaviour) or, for a workspace admin, for every booking whoever hosts it
-- ('all'). Without it a CRM sync has to be registered under every host's account, and a
-- host it was never added for is skipped silently. Validated in Go (webhook.ValidScope),
-- not with a CHECK, so the Down migration can drop the column.
ALTER TABLE webhooks ADD COLUMN scope TEXT NOT NULL DEFAULT 'own';

-- +goose Down
ALTER TABLE webhooks DROP COLUMN scope;
