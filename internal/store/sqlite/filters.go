package sqlite

import (
	"context"
	"strings"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

// Only adapter-owned SQL fragments enter clauses. All filter values are bound.
type predicates struct {
	clauses []string
	args    []any
}

func (p *predicates) add(clause string, args ...any) {
	p.clauses = append(p.clauses, clause)
	p.args = append(p.args, args...)
}

func (p predicates) where() string {
	if len(p.clauses) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(p.clauses, " AND ")
}

func (p *predicates) equal(column, value string) {
	if value != "" {
		p.add(column+"=?", value)
	}
}

func in[T ~string](p *predicates, column string, values []T) {
	if len(values) == 0 {
		return
	}
	// json_each keeps even very large lists within SQLite's parameter limit.
	p.add(column+" IN (SELECT value FROM json_each(?))", jsonField{values})
}

// For a millisecond-resolution column, x < bound and x >= bound both
// require rounding a sub-millisecond boundary upward.
func ceilMillis(t time.Time) int64 {
	ms := t.UnixMilli()
	if t.Nanosecond()%1_000_000 != 0 {
		ms++
	}
	return ms
}

func (p *predicates) timeBound(clause string, t time.Time) {
	if !t.IsZero() {
		p.add(clause, ceilMillis(t))
	}
}

func (p *predicates) page(page core.Page) string {
	limit := page.Limit
	if limit <= 0 {
		limit = -1
	}
	p.args = append(p.args, limit, page.Offset)
	return " LIMIT ? OFFSET ?"
}

func paymentFilter(f core.PaymentFilter) predicates {
	var p predicates
	in(&p, "status", f.Statuses)
	if f.NeedsAttention != nil {
		p.add("needs_attention=?", *f.NeedsAttention)
	}
	p.equal("order_id", f.OrderID)
	p.equal("link_id", f.LinkID)
	if f.FromLink != nil {
		if *f.FromLink {
			p.add("link_id<>''")
		} else {
			p.add("link_id=''")
		}
	}
	if f.Contract != "" {
		p.add("asset_contract=?", f.Contract)
	}
	if f.ChainID != 0 {
		p.add("chain_id=?", unsigned(f.ChainID))
	}
	p.timeBound("created_at>=?", f.CreatedFrom)
	p.timeBound("created_at<?", f.CreatedTo)
	if !f.PaidFrom.IsZero() || !f.PaidTo.IsZero() {
		p.add("paid_at<>?", time.Time{}.UnixMilli())
	}
	p.timeBound("paid_at>=?", f.PaidFrom)
	p.timeBound("paid_at<?", f.PaidTo)
	if !f.ExpiresBefore.IsZero() {
		p.add("expires_at<=?", f.ExpiresBefore.UnixMilli())
	}
	if !f.WatchedAfter.IsZero() {
		p.add("watch_until>?", f.WatchedAfter.UnixMilli())
	}
	if !f.WatchEndsBy.IsZero() {
		p.add("watch_until<=?", f.WatchEndsBy.UnixMilli())
	}
	if f.Retired != nil {
		if *f.Retired {
			p.add("retired_at<>?", time.Time{}.UnixMilli())
		} else {
			p.add("retired_at=?", time.Time{}.UnixMilli())
		}
	}
	if f.RetiredUnchecked {
		p.add("retired_unchecked=?", 1)
	}
	if f.Search != "" {
		needle := strings.ToLower(f.Search)
		// instr is a literal substring match, including percent and underscore.
		p.add(`(instr(hmnpay_lower(id), ?) OR instr(hmnpay_lower(order_id), ?)
 OR instr(hmnpay_lower(description), ?) OR instr(hmnpay_lower(customer_email), ?)
 OR instr(hmnpay_lower(address), ?) OR EXISTS (
 SELECT 1 FROM transfers WHERE payment_id=payments.id AND
 (instr(hmnpay_lower(tx_hash), ?) OR instr(hmnpay_lower(sender), ?))))`, needle, needle, needle, needle, needle, needle, needle)
	}
	return p
}

func (t transaction) ListPayments(ctx context.Context, f core.PaymentFilter, s core.PaymentSort, page core.Page) ([]core.Payment, error) {
	p := paymentFilter(f)
	col := "created_at"
	if s.By == core.SortByAmount {
		col = "amount_key"
	}
	dir := " ASC"
	if s.Desc {
		dir = " DESC"
	}
	query := selectPayment + p.where() + " ORDER BY " + col + dir + ", id" + dir + p.page(page)
	return many(ctx, t, scanPayment, query, p.args...)
}

func (t transaction) CountPayments(ctx context.Context, f core.PaymentFilter) (int, error) {
	p := paymentFilter(f)
	var n int
	err := t.QueryRowContext(ctx, "SELECT count(*) FROM payments"+p.where(), p.args...).Scan(&n)
	return n, err
}

func deliveryFilter(f core.DeliveryFilter) predicates {
	var p predicates
	p.equal("endpoint_id", f.EndpointID)
	p.equal("payment_id", f.PaymentID)
	p.equal("event_id", f.EventID)
	in(&p, "status", f.Statuses)
	p.timeBound("created_at>=?", f.CreatedFrom)
	if !f.DueBefore.IsZero() {
		p.add("next_attempt_at<>? AND next_attempt_at<=?", time.Time{}.UnixMilli(), f.DueBefore.UnixMilli())
	}
	if f.Unattempted != nil {
		if *f.Unattempted {
			p.add("attempt=0 AND event_seq>0")
		} else {
			p.add("NOT (attempt=0 AND event_seq>0)")
		}
	}
	return p
}

func (t transaction) ListDeliveries(ctx context.Context, f core.DeliveryFilter, page core.Page) ([]core.WebhookDelivery, error) {
	p := deliveryFilter(f)
	order := " ORDER BY created_at DESC, id DESC"
	switch {
	case !f.DueBefore.IsZero():
		order = " ORDER BY next_attempt_at, id"
	case f.Unattempted != nil && *f.Unattempted:
		order = " ORDER BY event_seq, id"
	}
	query := selectWebhookDelivery + p.where() + order + p.page(page)
	return many(ctx, t, scanWebhookDelivery, query, p.args...)
}

func (t transaction) CountDeliveries(ctx context.Context, f core.DeliveryFilter) (int, error) {
	p := deliveryFilter(f)
	var n int
	err := t.QueryRowContext(ctx, "SELECT count(*) FROM deliveries"+p.where(), p.args...).Scan(&n)
	return n, err
}

func (t transaction) ListEvents(ctx context.Context, f core.EventFilter) ([]core.Event, error) {
	var p predicates
	p.add("seq>?", f.After)
	p.equal("payment_id", f.PaymentID)
	in(&p, "type", f.Types)
	query := selectEvent + p.where() + " ORDER BY seq" + p.page(core.Page{Limit: f.Limit})
	return many(ctx, t, scanEvent, query, p.args...)
}
