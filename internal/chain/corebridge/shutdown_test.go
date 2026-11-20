package corebridge

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
	"github.com/lubluniky/hmnpay/internal/wallet"
)

// cancellingStore cancels the bridge's context during the first read, as a
// shutdown arriving between Run's context check and its ChainState call
// does.
type cancellingStore struct {
	core.Store
	once   sync.Once
	cancel context.CancelFunc
}

func (s *cancellingStore) View(ctx context.Context, fn func(core.Reader) error) error {
	s.once.Do(s.cancel)
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.View(ctx, fn)
}

// A shutdown is not an error: Run returns promptly and logs nothing at
// error level.
func TestShutdownDuringStateReadIsQuiet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc, err := core.New(core.Config{
		Store:     &cancellingStore{Store: coretest.NewMemStore(), cancel: cancel},
		Addresses: wallet.NewAddressSource(),
	})
	if err != nil {
		t.Fatal(err)
	}
	logs := &safeBuffer{}
	b, err := New(svc, &checkpoints{}, Options{
		Logger:     slog.New(slog.NewTextHandler(logs, nil)),
		MaxBackoff: time.Minute, // a wrongly taken backoff would show as a slow return
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := b.Run(ctx); err != context.Canceled {
		t.Fatalf("Run = %v", err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("Run took %s to notice the shutdown", d)
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("shutdown logged as an error:\n%s", logs.String())
	}
}
