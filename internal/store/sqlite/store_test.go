package sqlite

import (
	"path/filepath"
	"testing"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

func openTestDB(t *testing.T) core.Store {
	t.Helper()
	return openAt(t, filepath.Join(t.TempDir(), "hmnpay.db"))
}

func TestStoreSuite(t *testing.T) { coretest.RunStoreSuite(t, openTestDB) }

func TestServiceSuite(t *testing.T) { coretest.RunServiceSuite(t, openTestDB) }
