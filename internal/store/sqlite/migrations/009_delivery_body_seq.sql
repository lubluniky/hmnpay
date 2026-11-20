-- Webhook bodies carry the event's position in the event log as "seq"
-- (core.EventEnvelope.Seq), for the receiver to discard stale snapshots by.
-- Deliveries queued before that were given the position by 008
-- (event_seq) but kept their stored body, and a body is what is signed and
-- sent — on every retry and every resend. Here they get it: the very bytes
-- the current version writes for the same event, whose envelope has "seq"
-- as its last member. The event log is never pruned, so every delivery of a
-- logged event has its position; the test event (event_seq 0) has none and
-- keeps its body. (CASE, not AND: a body that is not JSON must not reach
-- json_type.)
UPDATE deliveries
SET request_body = substr(request_body, 1, length(request_body) - 1) || ',"seq":' || event_seq || '}'
WHERE event_seq > 0
  AND CASE WHEN json_valid(request_body)
      THEN json_type(request_body) = 'object' AND json_type(request_body, '$.seq') IS NULL
      ELSE 0 END
  AND substr(request_body, -1, 1) = '}';
