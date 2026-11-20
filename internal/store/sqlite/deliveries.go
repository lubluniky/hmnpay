package sqlite

import (
	"context"

	"github.com/lubluniky/hmnpay/core"
)

const selectWebhookDelivery = `SELECT id, endpoint_id, event_id, event_type, payment_id, url,
status, attempt, max_attempts, next_attempt_at, request_body, response_code,
response_body, duration_ms, error, attempts, created_at, updated_at,
completed_at, claim_id, event_seq FROM deliveries`

func scanWebhookDelivery(row scanner) (core.WebhookDelivery, error) {
	var v core.WebhookDelivery
	err := row.Scan(
		&v.ID, &v.EndpointID, &v.EventID,
		&v.EventType, &v.PaymentID, &v.URL,
		&v.Status, &v.Attempt, &v.MaxAttempts,
		(*timestamp)(&v.NextAttemptAt), &v.RequestBody, &v.ResponseCode,
		&v.ResponseBody, &v.DurationMs, &v.Error,
		jsonField{&v.Attempts}, (*timestamp)(&v.CreatedAt), (*timestamp)(&v.UpdatedAt),
		(*timestamp)(&v.CompletedAt), &v.ClaimID, &v.EventSeq,
	)
	for i := range v.Attempts {
		v.Attempts[i].At = v.Attempts[i].At.UTC()
	}
	return v, mapped(err)
}

func (t transaction) GetDelivery(ctx context.Context, value string) (core.WebhookDelivery, error) {
	return scanWebhookDelivery(t.QueryRowContext(ctx, selectWebhookDelivery+" WHERE id=?", value))
}

func webhookDeliveryValues(v core.WebhookDelivery) []any {
	return []any{
		v.ID, v.EndpointID, v.EventID,
		v.EventType, v.PaymentID, v.URL,
		v.Status, v.Attempt, v.MaxAttempts,
		v.NextAttemptAt.UnixMilli(), v.RequestBody, v.ResponseCode,
		v.ResponseBody, v.DurationMs, v.Error,
		jsonField{v.Attempts}, v.CreatedAt.UnixMilli(), v.UpdatedAt.UnixMilli(),
		v.CompletedAt.UnixMilli(), v.ClaimID, v.EventSeq,
	}
}

func (t transaction) InsertDelivery(ctx context.Context, v core.WebhookDelivery) error {
	_, err := t.ExecContext(ctx, `INSERT INTO deliveries (id, endpoint_id, event_id, event_type, payment_id, url,
status, attempt, max_attempts, next_attempt_at, request_body, response_code,
response_body, duration_ms, error, attempts, created_at, updated_at,
completed_at, claim_id, event_seq)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, webhookDeliveryValues(v)...)
	return mapped(err)
}

func (t transaction) UpdateDelivery(ctx context.Context, v core.WebhookDelivery) error {
	return t.replace(ctx, `UPDATE deliveries SET
endpoint_id=?2, event_id=?3, event_type=?4, payment_id=?5,
url=?6, status=?7, attempt=?8, max_attempts=?9,
next_attempt_at=?10, request_body=?11, response_code=?12, response_body=?13,
duration_ms=?14, error=?15, attempts=?16, created_at=?17,
updated_at=?18, completed_at=?19, claim_id=?20, event_seq=?21
WHERE id=?1`, webhookDeliveryValues(v)...)
}
