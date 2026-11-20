package sqlite

import (
	"context"

	"github.com/lubluniky/hmnpay/core"
)

const selectWebhookEndpoint = `SELECT id, url, description, events, enabled, secret,
previous_secret, previous_secret_expires_at, disabled_at, disabled_reason, failing_since, consecutive_failures,
last_delivery_at, last_success_at, created_at, updated_at FROM endpoints`

func scanWebhookEndpoint(row scanner) (core.WebhookEndpoint, error) {
	var v core.WebhookEndpoint
	err := row.Scan(
		&v.ID, &v.URL, &v.Description,
		jsonField{&v.Events}, &v.Enabled, &v.Secret,
		&v.PreviousSecret, (*timestamp)(&v.PreviousSecretExpiresAt), (*timestamp)(&v.DisabledAt),
		&v.DisabledReason, (*timestamp)(&v.FailingSince), &v.ConsecutiveFailures,
		(*timestamp)(&v.LastDeliveryAt), (*timestamp)(&v.LastSuccessAt), (*timestamp)(&v.CreatedAt),
		(*timestamp)(&v.UpdatedAt),
	)
	return v, mapped(err)
}

func (t transaction) GetWebhookEndpoint(ctx context.Context, value string) (core.WebhookEndpoint, error) {
	return scanWebhookEndpoint(t.QueryRowContext(ctx, selectWebhookEndpoint+" WHERE id=?", value))
}

func (t transaction) ListWebhookEndpoints(ctx context.Context) ([]core.WebhookEndpoint, error) {
	return many(ctx, t, scanWebhookEndpoint, selectWebhookEndpoint+" ORDER BY created_at, id")
}

func webhookEndpointValues(v core.WebhookEndpoint) []any {
	return []any{
		v.ID, v.URL, v.Description,
		jsonField{v.Events}, v.Enabled, v.Secret,
		v.PreviousSecret, v.PreviousSecretExpiresAt.UnixMilli(), v.DisabledAt.UnixMilli(),
		v.DisabledReason, v.FailingSince.UnixMilli(), v.ConsecutiveFailures,
		v.LastDeliveryAt.UnixMilli(), v.LastSuccessAt.UnixMilli(), v.CreatedAt.UnixMilli(),
		v.UpdatedAt.UnixMilli(),
	}
}

func (t transaction) InsertWebhookEndpoint(ctx context.Context, v core.WebhookEndpoint) error {
	_, err := t.ExecContext(ctx, `INSERT INTO endpoints (id, url, description, events, enabled, secret,
previous_secret, previous_secret_expires_at, disabled_at, disabled_reason, failing_since, consecutive_failures,
last_delivery_at, last_success_at, created_at, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, webhookEndpointValues(v)...)
	return mapped(err)
}

func (t transaction) UpdateWebhookEndpoint(ctx context.Context, v core.WebhookEndpoint) error {
	return t.replace(ctx, `UPDATE endpoints SET
url=?2, description=?3, events=?4, enabled=?5,
secret=?6, previous_secret=?7, previous_secret_expires_at=?8, disabled_at=?9,
disabled_reason=?10, failing_since=?11, consecutive_failures=?12, last_delivery_at=?13,
last_success_at=?14, created_at=?15, updated_at=?16
WHERE id=?1`, webhookEndpointValues(v)...)
}
