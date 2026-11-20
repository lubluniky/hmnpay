-- core.WebhookDelivery.EventSeq: the position of the delivered event in the
-- event log. First attempts to one endpoint are made in this order, and the
-- webhook body carries it as "seq". Existing deliveries get the seq of
-- their event where the event is still in the log (the test event, which
-- never is, keeps 0).
ALTER TABLE deliveries ADD COLUMN event_seq INTEGER NOT NULL DEFAULT 0;
UPDATE deliveries SET event_seq = COALESCE((SELECT seq FROM events WHERE events.id = deliveries.event_id), 0);
-- The head of each endpoint's queue of first attempts.
CREATE INDEX deliveries_first_attempts ON deliveries (endpoint_id, attempt, event_seq);
