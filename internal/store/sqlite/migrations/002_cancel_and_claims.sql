-- Payment cancellation (core.Payment.CancelledAt / CancelledBy) and webhook
-- claim fencing (core.WebhookDelivery.ClaimID). The integer default is the
-- millisecond encoding of the zero time, as used by every timestamp column.
ALTER TABLE payments ADD COLUMN cancelled_at INTEGER NOT NULL DEFAULT -62135596800000;
ALTER TABLE payments ADD COLUMN cancelled_by TEXT NOT NULL DEFAULT '';
ALTER TABLE deliveries ADD COLUMN claim_id TEXT NOT NULL DEFAULT '';

CREATE INDEX sessions_created_at ON sessions (created_at, id);
