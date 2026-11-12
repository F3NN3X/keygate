package store_test

import (
	"context"
	"testing"

	"github.com/tabloy/keygate/internal/store"
	"github.com/uptrace/bun"
)

// A statement that failed aborts the transaction, and Postgres turns
// the COMMIT into a rollback that pgdriver reports as success. The
// helpers say so instead, even when the closure dropped the error.
func TestAbortedTransactionIsAnError(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	swallow := func(ctx context.Context, tx bun.Tx) error {
		_, _ = tx.ExecContext(ctx, "SELECT 1/0")
		return nil
	}
	if err := store.RunInTx(ctx, s.DB, swallow); err == nil {
		t.Fatal("RunInTx: an aborted transaction was reported as committed")
	}
	if err := s.WithXactLock(ctx, 987654321, swallow); err == nil {
		t.Fatal("WithXactLock: an aborted transaction was reported as committed")
	}
	ok := func(ctx context.Context, tx bun.Tx) error {
		_, err := tx.ExecContext(ctx, "SELECT 1")
		return err
	}
	if err := store.RunInTx(ctx, s.DB, ok); err != nil {
		t.Fatalf("RunInTx: %v", err)
	}
	if err := s.WithXactLock(ctx, 987654321, ok); err != nil {
		t.Fatalf("WithXactLock: %v", err)
	}
}
