// Package hmnpay is the embeddable hmnpay engine: a self-hosted,
// non-custodial crypto payment gateway (USDT on BNB Smart Chain) in one Go
// value. The hmnpay server binary (cmd/hmnpay) is a thin shell around it;
// a Telegram bot, a CLI or any other Go program can embed it the same way.
//
//	eng, err := hmnpay.Open(ctx, hmnpay.Config{DataDir: "/var/lib/hmnpay"})
//	if err != nil { … }
//	defer eng.Close()
//	go eng.Run(ctx) // chain watcher, webhook dispatcher, clean-up
//
//	svc := eng.Service() // *core.Service: every use case
//	bot := core.WithActor(ctx, core.SystemActor("telegram-bot"))
//	p, _, err := svc.CreatePayment(bot, core.CreatePaymentInput{Amount: "25.00"})
//
// Everything goes through [core.Service]; authorisation lives in the core,
// so an embedder cannot bypass a rule a transport would enforce. Calls are
// made as an actor: [core.SystemActor] for trusted in-process code, or the
// actor returned by [core.Service.AuthenticateAPIKey] /
// [core.Service.AuthenticateSession] when acting for someone else.
//
// Outbound, every consumer reads the same durable event log: keep a cursor
// ([core.Service.Cursor] / [core.Service.SaveCursor]), read with
// [core.Service.Events] (at-least-once), and wait on [core.Service.Subscribe]
// for a nudge instead of polling. See the example.
//
// The HTTP API is optional: [Engine.HTTPHandler] returns the same REST API,
// checkout stream and health probes the server binary serves, ready to be
// mounted on any http.Server. Workers can be switched off one by one in
// [Config] (an embedder that only reads, or a test without a chain).
//
// All state is in [Config.DataDir] (a SQLite database in WAL mode). The
// first-run setup (operator account, network, RPC endpoints, xpub) is a use
// case like any other ([core.Service.CompleteSetup]), normally completed in
// the web wizard. Until it is, the HTTP API demands a setup token from
// anonymous callers so that a stranger who reaches the port first cannot
// claim the instance: [Open] generates one unless [Config.SetupToken] is
// given ([Engine.SetupToken] returns it; show it to the person installing),
// and [Config.NoSetupToken] is the explicit opt-out.
package hmnpay
