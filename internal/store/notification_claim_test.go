package store_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tabloy/keygate/internal/model"
)

// TryRecordNotification is the claim notifyPaymentRecovered
// takes before it sends: of callers racing on one tag exactly one gets
// true, and a later call gets false.
func TestTryRecordNotificationClaimsOnce(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	lic := newLicenseOn(t, s, ctx, "subscription", model.StatusActive, nil)

	var won atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.TryRecordNotification(ctx, lic.ID, "payment_recovered:1")
			if err != nil {
				t.Error(err)
			}
			if ok {
				won.Add(1)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("%d callers claimed the tag, want exactly 1", won.Load())
	}
	if ok, _ := s.TryRecordNotification(ctx, lic.ID, "payment_recovered:1"); ok {
		t.Fatal("a later call claimed a tag already recorded")
	}
	if ok, _ := s.TryRecordNotification(ctx, lic.ID, "payment_recovered:2"); !ok {
		t.Fatal("another episode's tag was refused")
	}
}
