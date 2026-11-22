// Package storetest provides an isolated, migrated database for tests.
package storetest

import (
	"context"
	"os"
	"testing"

	"github.com/VoteForTruth/vft-indexer/internal/store"
)

// Open returns a store on a fresh Postgres schema named `schema`, so test packages
// running in parallel don't interfere. The test is skipped without a database.
// VFT_TEST_DSN overrides the default local database.
func Open(t *testing.T, schema string) *store.Store {
	t.Helper()
	ctx := context.Background()

	dsn := os.Getenv("VFT_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://localhost/vft_indexer_test?sslmode=disable"
	}

	admin, err := store.Open(ctx, dsn)
	if err != nil {
		t.Skipf("no test database: %v", err)
	}
	_, err = admin.Pool().Exec(ctx, `drop schema if exists `+schema+` cascade; create schema `+schema)
	admin.Close()
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, dsn+"&search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)

	if err = st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}
