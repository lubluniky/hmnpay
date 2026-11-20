package watcher_test

import (
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// headChecked returns Head.Checked, and whether the watcher reports it (the
// randomised model retires addresses by it).
func headChecked(h watcher.Head) (time.Time, bool) { return h.Checked, true }
