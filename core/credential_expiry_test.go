package core_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

// gatedStore holds the next write transaction at the door — as the store's
// single writer does while another transaction runs — until it is let in.
type gatedStore struct {
	core.Store
	mu      sync.Mutex
	gate    chan struct{} // closed to let the held transaction in
	waiting chan struct{} // closed once a transaction is held
}

// hold makes the next write transaction wait: waiting is closed when it
// does, and it goes on when gate is closed.
func (g *gatedStore) hold() (gate, waiting chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gate, g.waiting = make(chan struct{}), make(chan struct{})
	return g.gate, g.waiting
}

func (g *gatedStore) Update(ctx context.Context, fn func(core.Tx) error) error {
	g.mu.Lock()
	gate, waiting := g.gate, g.waiting
	g.gate, g.waiting = nil, nil
	g.mu.Unlock()
	if gate != nil {
		close(waiting)
		<-gate
	}
	return g.Store.Update(ctx, fn)
}

// TestCredentialThatExpiresWhileAWriteWaitsWritesNothing: every write checks,
// in its own transaction, that the session or API key the caller was
// authenticated with still holds — by the time of the write. A request that
// was authenticated a moment before its credential ran out, and then waited
// for the store's writer until after that, changes nothing.
func TestCredentialThatExpiresWhileAWriteWaitsWritesNothing(t *testing.T) {
	type credential struct {
		name   string
		actor  func(e *coretest.Env) context.Context
		expire time.Duration // from now to past the credential's end
	}
	for _, c := range []credential{
		{"a session that runs idle", func(e *coretest.Env) context.Context { return e.Relogin() }, core.DefaultSessionIdleTimeout + time.Second},
		{"an API key that expires", func(e *coretest.Env) context.Context {
			_, secret, err := e.Service.CreateAPIKey(e.Relogin(), core.CreateAPIKeyInput{Name: "short", Scopes: core.Scopes, ExpiresAt: e.Clock.Now().Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			actor, err := e.Service.AuthenticateAPIKey(context.Background(), secret)
			if err != nil {
				t.Fatal(err)
			}
			return core.WithActor(context.Background(), actor)
		}, time.Hour + time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := &gatedStore{Store: coretest.NewMemStore()}
			e := coretest.NewEnv(t, coretest.WithStore(store))
			ctx := c.actor(e)
			gate, waiting := store.hold()
			done := make(chan error, 1)
			go func() {
				_, err := e.Service.CreateLink(ctx, core.LinkInput{Name: "Written after the credential ended", Active: true})
				done <- err
			}()
			<-waiting // authenticated, in time, and waiting for the writer
			e.Clock.Advance(c.expire)
			close(gate)
			if err := <-done; err == nil || core.KindOf(err) != core.KindUnauthorized {
				t.Fatalf("a write that waited until after its credential ended: %v", err)
			}
			links, err := e.Service.ListLinks(e.Relogin())
			if err != nil || len(links) != 0 {
				t.Fatalf("links: %+v, %v", links, err)
			}
		})
	}
}

// TestSessionThatRunsIdleWhileItIsTouchedIsNotRevived: authenticating a
// session marks it as used (at most every few minutes), in a write. A
// session whose idle timeout runs out while that write waits for the store
// is not marked used — that would give it another day — and is not let in.
func TestSessionThatRunsIdleWhileItIsTouchedIsNotRevived(t *testing.T) {
	store := &gatedStore{Store: coretest.NewMemStore()}
	e := coretest.NewEnv(t, coretest.WithStore(store))
	e.Clock.Advance(core.DefaultSessionIdleTimeout - time.Second) // still good, and due to be touched
	gate, waiting := store.hold()
	done := make(chan error, 1)
	go func() {
		_, err := e.Service.AuthenticateSession(context.Background(), e.Token)
		done <- err
	}()
	<-waiting
	e.Clock.Advance(2 * time.Second)
	close(gate)
	if err := <-done; core.KindOf(err) != core.KindUnauthorized {
		t.Fatalf("a session that ran idle while it was being touched: %v", err)
	}
	if _, err := e.Service.AuthenticateSession(context.Background(), e.Token); core.KindOf(err) != core.KindUnauthorized {
		t.Fatalf("the session was revived: %v", err)
	}
}
