package hmnpay_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/lubluniky/hmnpay"
	"github.com/lubluniky/hmnpay/core"
)

// A non-HTTP consumer in the style of a Telegram bot: it embeds the engine,
// creates payments as a trusted in-process actor, and follows the durable
// event log with a saved cursor, so that after a restart it resumes where
// it stopped and never misses a payment.
//
// The chain watcher is switched off here (no network in examples); the
// example reports the on-chain facts itself, exactly as the watcher would.
func Example_telegramBot() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir, _ := os.MkdirTemp("", "hmnpay-example-")
	defer os.RemoveAll(dir)

	eng, err := hmnpay.Open(ctx, hmnpay.Config{
		DataDir:        dir,
		Logger:         slog.New(slog.DiscardHandler),
		Chain:          hmnpay.ChainConfig{Disabled: true}, // a real bot leaves it on
		Webhooks:       hmnpay.WebhookConfig{Disabled: true},
		PasswordHasher: core.PBKDF2Hasher{Iterations: 1000}, // fast for the example only
	})
	if err != nil {
		panic(err)
	}
	defer eng.Close()
	runDone := make(chan error, 1)
	go func() { runDone <- eng.Run(ctx) }()
	svc := eng.Service()

	// First-run setup, normally done in the web wizard, where the person
	// installing must enter eng.SetupToken(). In-process code acts as a
	// system actor, which needs no token.
	bot := core.WithActor(ctx, core.SystemActor("telegram-bot"))
	if _, err := svc.CompleteSetup(bot, core.SetupInput{
		Email: "owner@example.com", Password: "correct horse battery staple",
		RPCURLs:  []string{"https://bsc-rpc.publicnode.com", "https://bsc-dataseed.bnbchain.org"},
		XPub:     "xpub6Ce9NcJvTk36xtLSrJLZqE7wtgA5deCeYs7rSQtreh4cj6ByPtrg9sD7V2FNFLPnf8heNP3FGkeV9qwfzvZNSd54JoNXVsXFYSYwHsnJxqP",
		Merchant: core.MerchantSettings{Name: "Northwind", SupportEmail: "help@example.com"}, PublicURL: "https://pay.example.com",
	}); err != nil {
		panic(err)
	}

	const consumer = "telegram"

	// The bot's notifier: follow the event log from the saved cursor.
	paid := make(chan string)
	go func() {
		sub := svc.Subscribe() // a nudge, so the loop does not poll
		defer sub.Close()
		cursor, err := svc.Cursor(bot, consumer)
		if err != nil {
			panic(err)
		}
		for {
			events, err := svc.Events(bot, core.EventFilter{After: cursor, Types: []core.EventType{core.EventPaymentCreated, core.EventPaymentPaid}})
			if err != nil {
				panic(err)
			}
			for _, e := range events {
				p, _ := e.Payment()
				switch e.Type {
				case core.EventPaymentCreated:
					fmt.Printf("new invoice %s: %s %s\n", p.OrderID, p.Amount, p.Asset)
				case core.EventPaymentPaid:
					fmt.Printf("paid %s: %s %s\n", p.OrderID, p.AmountReceived, p.Asset)
				}
				// Process, then advance: at-least-once delivery.
				cursor = e.Seq
				if err := svc.SaveCursor(bot, consumer, cursor); err != nil {
					panic(err)
				}
				if e.Type == core.EventPaymentPaid {
					paid <- p.ID
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-sub.C():
			case <-time.After(5 * time.Second):
			}
		}
	}()

	// What the chain watcher reports first: how far it has processed the
	// chain. No deposit address is handed out before (core.ErrChainNotReady).
	hash := func(n uint64) string { return fmt.Sprintf("0x%064x", n) }
	watcher := core.WithActor(ctx, core.SystemActor("chain-watcher"))
	if err := svc.AdvanceHead(watcher, core.ChainHead{ChainID: core.ChainBSCMainnet, Number: 990, Hash: hash(990), Time: time.Now(), Final: 990}); err != nil {
		panic(err)
	}

	// A chat command creates an invoice.
	p, _, err := svc.CreatePayment(bot, core.CreatePaymentInput{Amount: "25.00", OrderID: "chat-42"})
	if err != nil {
		panic(err)
	}

	// Then: a transfer to the deposit address, and the chain advancing past
	// the required confirmations.
	if err := svc.ObserveTransfer(watcher, core.TransferObservation{
		ChainID: core.ChainBSCMainnet, Contract: core.USDTBSC.Contract,
		TxHash: hash(0xabc), LogIndex: 0, BlockNumber: 1000, BlockHash: hash(1000),
		From: "0x8894e0a0c962cb723c1976a4421c95949be2d4e3", To: p.Address,
		Amount: core.MustParseAmount("25", core.USDTBSC.Decimals),
	}); err != nil {
		panic(err)
	}
	head := uint64(1000 + p.RequiredConfirmations - 1)
	if err := svc.AdvanceHead(watcher, core.ChainHead{ChainID: core.ChainBSCMainnet, Number: head, Hash: hash(head), Time: time.Now(), Final: head}); err != nil {
		panic(err)
	}

	<-paid
	cursor, _ := svc.Cursor(bot, consumer)
	fmt.Println("cursor saved:", cursor > 0)

	cancel()
	if err := <-runDone; err != nil {
		panic(err)
	}

	// Output:
	// new invoice chat-42: 25.00 USDT
	// paid chat-42: 25.00 USDT
	// cursor saved: true
}
