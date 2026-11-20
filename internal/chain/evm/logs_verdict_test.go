package evm_test

import (
	"context"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
)

// "this endpoint has no log service" is a verdict that takes three
// refusals in a row, a minute and then two minutes apart — one refusal is
// what a throttling provider looks like too. It ends with the first log
// query the endpoint answers, and a caller can hand it down to a new
// process.
func TestNoLogServiceVerdict(t *testing.T) {
	ctx := context.Background()
	query := evm.TransferQuery{Token: usdt, FromBlock: 1090, ToBlock: 1100, Recipients: []ethaddr.Address{addr(1)}}
	refuse := func(b *evmtest.Behavior) { b.LogsError = &evmtest.RPCError{Code: -32005, Message: "limit exceeded"} }

	p := newPool(t, 1)
	ep, _ := p.client.Pick(ctx)
	p.nodes[0].Set(refuse)
	for round := 1; round <= evm.LogsAbsentRefusals+1; round++ {
		if _, err := scanAll(ctx, ep, query); err == nil {
			t.Fatalf("round %d: the log query was not refused", round)
		}
		logs := ep.Health().Logs
		if logs.Refusals != round || !logs.Unavailable || logs.Known || logs.NoService != (round >= evm.LogsAbsentRefusals) {
			t.Fatalf("after refusal %d: %+v", round, logs)
		}
		if want := time.Minute << (round - 1); !logs.UnavailableUntil.Equal(p.clock.Now().Add(want)) {
			t.Fatalf("after refusal %d the endpoint is left alone until %s, want %s from now", round, logs.UnavailableUntil, want)
		}
		p.clock.Advance(time.Minute<<(round-1) + time.Second)
		if logs := ep.Health().Logs; logs.Unavailable || logs.NoService != (round >= evm.LogsAbsentRefusals) {
			t.Fatalf("after the pause that follows refusal %d: %+v (the verdict stands, the endpoint may be asked)", round, logs)
		}
	}
	// One answer ends it, for good: from now on a refusal is throttling.
	p.nodes[0].Set(func(b *evmtest.Behavior) { b.LogsError = nil })
	if _, err := scanAll(ctx, ep, query); err != nil {
		t.Fatal(err)
	}
	if logs := ep.Health().Logs; logs.NoService || logs.Unavailable || !logs.Known || logs.Refusals != 0 {
		t.Fatalf("after an answer: %+v", logs)
	}
	p.nodes[0].Set(refuse)
	for i := 0; i < 5; i++ {
		_, _ = scanAll(ctx, ep, query)
		p.clock.Advance(time.Hour)
	}
	if logs := ep.Health().Logs; logs.NoService || !logs.Known {
		t.Fatalf("an endpoint that has served logs came under the verdict: %+v", logs)
	}

	// The verdict, handed down: it stands at once, the endpoint is asked all
	// the same, and it cannot be put on an endpoint known to serve logs.
	q := newPool(t, 2)
	first, _ := q.client.Pick(ctx)
	second, _ := q.client.Pick(ctx, evm.Except(first))
	first.MarkLogsAbsent()
	if logs := first.Health().Logs; !logs.NoService || logs.Unavailable || logs.Known {
		t.Fatalf("after MarkLogsAbsent: %+v", logs)
	}
	if _, err := scanAll(ctx, first, query); err != nil {
		t.Fatal(err)
	}
	if logs := first.Health().Logs; logs.NoService || !logs.Known {
		t.Fatalf("an endpoint remembered as having no log service answered a log query: %+v", logs)
	}
	second.MarkLogsKnown()
	second.MarkLogsAbsent()
	if logs := second.Health().Logs; logs.NoService || !logs.Known {
		t.Fatalf("MarkLogsAbsent on an endpoint known to serve logs: %+v", logs)
	}
}
