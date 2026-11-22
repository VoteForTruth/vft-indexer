// Package storetest provides an isolated, migrated database for tests.
package storetest

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/VoteForTruth/vft-indexer/internal/store"
)

// Open returns a store on a fresh SQLite file in the test's temp directory, so
// tests never share data.
func Open(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)

	if err = st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}
