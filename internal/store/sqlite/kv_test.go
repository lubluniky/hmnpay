package sqlite

import (
	"bytes"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/lubluniky/hmnpay/core"
)

func TestCheckpointRoundTrip(t *testing.T) {
	for _, path := range []string{":memory:", filepath.Join(t.TempDir(), "kv.db")} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			ctx := t.Context()
			c := openAt(t, path).Checkpoints()
			if v, err := c.LoadCheckpoint(ctx); err != nil || v != nil {
				t.Fatalf("fresh store: %q, %v; want nil", v, err)
			}
			for _, want := range [][]byte{[]byte("first"), {0, 1, 2, 0xff}, bytes.Repeat([]byte("x"), 1<<20)} {
				if err := c.SaveCheckpoint(ctx, want); err != nil {
					t.Fatal(err)
				}
				got, err := c.LoadCheckpoint(ctx)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("got %d bytes, %v; want %d bytes", len(got), err, len(want))
				}
			}
			// The returned value is a private copy.
			got, _ := c.LoadCheckpoint(ctx)
			got[0] = 'y'
			if again, _ := c.LoadCheckpoint(ctx); again[0] != 'x' {
				t.Fatal("LoadCheckpoint returned shared memory")
			}
			// Saving an empty value makes the next load return empty.
			for _, empty := range [][]byte{nil, {}} {
				if err := c.SaveCheckpoint(ctx, []byte("something")); err != nil {
					t.Fatal(err)
				}
				if err := c.SaveCheckpoint(ctx, empty); err != nil {
					t.Fatal(err)
				}
				if v, err := c.LoadCheckpoint(ctx); err != nil || len(v) != 0 {
					t.Fatalf("after saving %#v: %q, %v; want empty", empty, v, err)
				}
			}
			// Deleting what is not there is fine.
			if err := c.SaveCheckpoint(ctx, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCheckpointSurvivesReopen(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "kv.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoints().SaveCheckpoint(ctx, []byte("cursor=123")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutValue(ctx, "other", []byte("independent")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openAt(t, path)
	if v, err := s.Checkpoints().LoadCheckpoint(ctx); err != nil || string(v) != "cursor=123" {
		t.Fatalf("after reopen: %q, %v", v, err)
	}
	if v, err := s.GetValue(ctx, "other"); err != nil || string(v) != "independent" {
		t.Fatalf("other key after reopen: %q, %v", v, err)
	}
}

// Checkpoint writes share the single write connection with core
// transactions, from this process and from another one on the same file:
// neither side sees an error or loses a write.
func TestCheckpointConcurrentWithCoreWrites(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "kv.db")
	a, b := openAt(t, path), openAt(t, path)
	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, 6*n)
	for _, s := range []*Store{a, b} {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range n {
				errs <- s.Update(ctx, func(tx core.Tx) error {
					c, err := tx.GetCursor(ctx, "counter")
					if err != nil {
						return err
					}
					return tx.SetCursor(ctx, "counter", c+1)
				})
			}
		}()
		go func() {
			defer wg.Done()
			for i := range n {
				errs <- s.Checkpoints().SaveCheckpoint(ctx, fmt.Appendf(nil, "checkpoint %d", i))
				_, err := s.Checkpoints().LoadCheckpoint(ctx)
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := a.View(ctx, func(r core.Reader) error { check(t, int64(2*n))(r.GetCursor(ctx, "counter")); return nil }); err != nil {
		t.Fatal(err)
	}
	if v, err := b.Checkpoints().LoadCheckpoint(ctx); err != nil || string(v) != fmt.Sprintf("checkpoint %d", n-1) {
		t.Fatalf("last checkpoint: %q, %v", v, err)
	}
}
