package core_test

import (
	"context"
	"fmt"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

// The whole system in one screen: wire the service to its ports, set the
// instance up, create a payment as a merchant, report chain facts as the
// watcher, and consume the event log as any outbound adapter would.
func Example() {
	clock := coretest.NewClock()
	svc, err := core.New(core.Config{
		Store:     coretest.NewMemStore(), // internal/store/sqlite in production
		Addresses: &coretest.Addresses{},  // internal/wallet in production
		Hasher:    coretest.Hasher{},      // core.PBKDF2Hasher by default
		Clock:     clock,
		Rand:      coretest.NewRand(1),
	})
	if err != nil {
		panic(err)
	}
	public := context.Background()

	// First-run setup (the wizard's final step).
	_, err = svc.CompleteSetup(public, core.SetupInput{
		Email: "owner@example.com", Password: "correct horse battery staple",
		ChainID: core.ChainBSCMainnet, RPCURLs: []string{"https://bsc-dataseed.bnbchain.org"},
		XPub:      coretest.XPub,
		Merchant:  core.MerchantSettings{Name: "Northwind", SupportEmail: "help@example.com"},
		PublicURL: "https://pay.example.com",
	})
	if err != nil {
		panic(err)
	}

	// The chain watcher reports facts; the core decides what they mean.
	// The first is how far it has processed the chain: until then no
	// deposit address is handed out (core.ErrChainNotReady).
	watcher := core.WithActor(public, core.SystemActor("chain-watcher"))
	blockHash := func(n uint64) string { return coretest.Hash("block", n) }
	if err := svc.AdvanceHead(watcher, core.ChainHead{ChainID: core.ChainBSCMainnet, Number: 90, Hash: blockHash(90), Time: clock.Now(), Final: 90}); err != nil {
		panic(err)
	}

	// Inbound: any transport authenticates, attaches the actor and calls
	// the service. Here: trusted in-process code, e.g. a Telegram bot.
	bot := core.WithActor(public, core.SystemActor("telegram-bot"))
	payment, _, err := svc.CreatePayment(bot, core.CreatePaymentInput{Amount: "149.00", OrderID: "ORD-1"})
	if err != nil {
		panic(err)
	}
	fmt.Println("created:", payment.Status, payment.Amount, payment.Asset)

	err = svc.ObserveTransfer(watcher, core.TransferObservation{
		ChainID: core.ChainBSCMainnet, Contract: core.USDTBSC.Contract,
		TxHash: coretest.Hash("tx"), LogIndex: 7, BlockNumber: 100, BlockHash: blockHash(100),
		From: coretest.Address("payer"), To: payment.Address,
		Amount: core.MustParseAmount("149", 18),
	})
	if err != nil {
		panic(err)
	}
	for _, n := range []uint64{100, 107, 114} {
		clock.Advance(3 * time.Second)
		if err := svc.AdvanceHead(watcher, core.ChainHead{ChainID: core.ChainBSCMainnet, Number: n, Hash: blockHash(n), Time: clock.Now(), Final: n}); err != nil {
			panic(err)
		}
		p, _ := svc.GetPayment(bot, payment.ID)
		fmt.Printf("head %d: %s %d/%d\n", n, p.Status, p.Confirmations, p.RequiredConfirmations)
	}

	// Outbound: every consumer reads the same log from its own cursor.
	var cursor int64
	events, err := svc.Events(bot, core.EventFilter{After: cursor, PaymentID: payment.ID})
	if err != nil {
		panic(err)
	}
	for _, e := range events {
		p, _ := e.Payment()
		fmt.Println("event:", e.Type, p.AmountReceived)
		cursor = e.Seq
	}

	// Output:
	// created: awaiting 149.00 USDT
	// head 100: confirming 1/15
	// head 107: confirming 8/15
	// head 114: paid 15/15
	// event: payment.created 0.00
	// event: payment.confirming 149.00
	// event: payment.paid 149.00
}
