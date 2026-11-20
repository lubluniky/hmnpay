package core_test

import (
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

// TestAnAddressStaysWhileAFactOfTheRetirementRuleIsUnknown: a cancelled
// invoice's address leaves the watch list only once the processed head and
// the time of its final block are past the end of its window. Heads that do
// not say when their final block was mined — from a chain adapter's record
// that predates the field, or a final block whose header has not been read —
// retire nothing, however far past the window the wall clock and the head's
// own time are: unknown is not "past". With the time known, it leaves.
func TestAnAddressStaysWhileAFactOfTheRetirementRuleIsUnknown(t *testing.T) {
	e := coretest.NewEnv(t)
	e.Head(100)
	p := e.CreatePayment("25")
	if _, err := e.Service.CancelPayment(e.Operator(), p.ID); err != nil {
		t.Fatal(err)
	}
	e.Clock.Advance(core.DefaultPaymentTTL + core.DefaultExpiryGrace + core.DefaultUnfundedWatchWindow + core.WatchRetirementMargin + time.Hour)
	advance := func(n, final uint64, finalTime time.Time) {
		t.Helper()
		head := core.ChainHead{ChainID: core.ChainBSCMainnet, Number: n, Hash: coretest.BlockHash(n, 0), Time: e.Clock.Now(), Final: final, FinalTime: finalTime}
		if err := e.Service.AdvanceHead(e.System(), head); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range []struct {
		name      string
		final     uint64
		finalTime time.Time
	}{
		{"the final block's time not known", 150, time.Time{}},
		{"no final block", 0, time.Time{}},
		{"a final time after the head's (not the final block's)", 150, e.Clock.Now().Add(time.Minute)},
	} {
		advance(200+uint64(i), c.final, c.finalTime)
		if !watched(t, e, p.ID) {
			t.Fatalf("%s: the address left the watch list", c.name)
		}
	}
	advance(300, 299, e.Clock.Now())
	if watched(t, e, p.ID) {
		t.Fatal("the final block's time known and past the window: the address is still watched")
	}
}
