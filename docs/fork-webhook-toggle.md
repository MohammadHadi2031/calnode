# Disable and re-enable a webhook (wisegrid fork)

A webhook can be switched off and on again from Webhooks (sidebar → Developer), without deleting it
and losing its URL, signing secret, events and field selection. No migration: it uses the
existing `webhooks.is_active` column, which had no way to be changed until now.

## What "disabled" means

- **Nothing new is queued** for it. `Enqueue` already only picked active webhooks.
- **Nothing already queued is sent.** A delivery queued before the switch was turned off,
  or one still waiting to retry, is closed as `failed` when the worker reaches it, and the
  endpoint is not called. This is what makes "off" mean "stop sending now", e.g. while the
  receiving system (n8n) is down or being changed.
- **Turning it back on does not replay** events from the time it was off. Only events after
  that are sent. A replay could re-create or re-cancel things in the receiving system long
  after the fact.

## API

`PATCH /v1/webhooks/{id}` with `{"is_active": false}` or `{"is_active": true}` (can be
combined with `events` and `fields`). `204` on success, `404` for someone else's webhook.
`GET /v1/webhooks` already returned `is_active`.

Code: `webhook.Service.Update` (`internal/webhook/webhook.go`), the `is_active` check in
`deliverWebhook` (`internal/worker/worker.go`), and the switch on the Webhooks page.
