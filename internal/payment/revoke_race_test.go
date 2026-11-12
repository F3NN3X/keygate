package payment

import (
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// A take-back that read the licence active, and
// whose revocation lands after another one committed, says nothing: the
// first revocation was told. The race of TestConcurrentTakeBacksRevokeOnce
// made certain: the row is locked while the refund reads it, and revoked
// before the lock is let go.
func TestTakeBackAfterAConcurrentRevocationTellsNothing(t *testing.T) {
	f := newUpgradeFixture(t, "revlock")
	pi := "pi_revlock_" + f.tag
	l := f.reload(t)
	l.PaymentProvider, l.StripePaymentIntentID = "stripe", pi
	if err := f.s.UpdateLicense(f.ctx, l, "payment_provider", "stripe_payment_intent_id"); err != nil {
		t.Fatal(err)
	}
	tx, err := f.s.DB.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(f.ctx, "SELECT id FROM licenses WHERE id = ? FOR UPDATE", l.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- f.h.takeBackCharge(f.ctx, refundedCharge{ID: "ch_revlock_" + f.tag, PaymentIntent: pi, Refunded: true}, "refund", time.Now().Unix())
	}()
	time.Sleep(700 * time.Millisecond) // the refund has read the licence active and waits on the lock
	if _, err := tx.ExecContext(f.ctx, "UPDATE licenses SET status = ? WHERE id = ?", model.StatusRevoked, l.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	var n int
	_ = f.s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = 'revoked'", l.ID).Scan(f.ctx, &n)
	if n != 0 {
		t.Fatalf("revoked told %d times by a refund that found the licence revoked under its lock, want 0", n)
	}
}
