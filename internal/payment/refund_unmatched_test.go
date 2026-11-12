package payment

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// A refunded charge that only the customer ties to the licence
// (no invoice, a payment intent that is not the licence's purchase) is
// only audited (refund_unmatched): the licence is not revoked and its
// subscription is not cancelled.
func TestRefundFoundThroughTheCustomerIsOnlyAudited(t *testing.T) {
	f := newUpgradeFixture(t, "oneonly")
	subID, cus := "sub_oa_"+f.tag, "cus_oa_"+f.tag
	l := f.reload(t)
	l.StripeSubscriptionID, l.PaymentProvider, l.StripeCustomerID = subID, "stripe", cus
	if err := f.s.UpdateLicense(f.ctx, l, "stripe_subscription_id", "payment_provider", "stripe_customer_id"); err != nil {
		t.Fatal(err)
	}
	before := l.Status
	var cancels atomic.Int32
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/invoice_payments":
			fmt.Fprint(w, `{"object":"list","has_more":false,"url":"/v1/invoice_payments","data":[]}`)
		case r.Method == http.MethodDelete:
			cancels.Add(1)
			fmt.Fprintf(w, `{"id":%q,"object":"subscription","status":"canceled"}`, subID)
		default:
			t.Errorf("unexpected Stripe call %s %s", r.Method, r.URL.Path)
			http.Error(w, `{"error":{"message":"unexpected"}}`, http.StatusNotFound)
		}
	})
	if err := f.h.onChargeRefunded(f.ctx, []byte(fmt.Sprintf(`{"id":"ch_oa_%s","customer":%q,"payment_intent":"pi_oa_%s","refunded":true}`, f.tag, cus, f.tag))); err != nil {
		t.Fatal(err)
	}
	if n := cancels.Load(); n != 0 {
		t.Fatalf("subscription cancelled %d times, want 0", n)
	}
	if st := f.reload(t).Status; st != before || st == model.StatusRevoked {
		t.Fatalf("status %s after a refund found only through the customer, want %s (only audited)", st, before)
	}
	var n int
	for range 30 {
		_ = f.s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = 'refund_unmatched'", l.ID).Scan(f.ctx, &n)
		if n > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n != 1 {
		t.Fatalf("%d refund_unmatched audit lines, want 1", n)
	}
}
