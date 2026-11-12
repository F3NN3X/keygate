package payment

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// invoice.payment_failed reads the subscription from Stripe for every
// invoice now, not only for a subscription change. A read
// that fails is retried: the event returns an error and the licence is
// left as it is, rather than moved to past_due from the payload.
func TestPaymentFailedRetriesAFailedStripeRead(t *testing.T) {
	s, ctx := openStore(t)
	t.Cleanup(func() { s.Close() })
	h := &StripeHandler{Store: s}
	plan := seedPlan(t, s, ctx, "pfread", "subscription")
	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"type":"invalid_request_error","message":"stubbed failure"}}`)
	})
	until := time.Now().Add(5 * 24 * time.Hour)
	lic := &model.License{ProductID: plan.ProductID, PlanID: plan.ID, Email: "pfread-" + suffix + "@example.com",
		LicenseKey: "KEY-PFREAD-" + suffix, Status: model.StatusActive, ValidUntil: &until,
		StripeSubscriptionID: "sub_pfread_" + suffix}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	err := h.onPaymentFailed(ctx, []byte(fmt.Sprintf(`{"subscription":%q,"status":"open","billing_reason":"subscription_cycle"}`, lic.StripeSubscriptionID)))
	if err == nil {
		t.Fatal("payment_failed with Stripe unreadable returned nil, want an error so Stripe retries")
	}
	var st string
	if err := s.DB.NewRaw("SELECT status FROM licenses WHERE id = ?", lic.ID).Scan(ctx, &st); err != nil {
		t.Fatal(err)
	}
	if st != model.StatusActive {
		t.Fatalf("status %s after a failed read, want active (left for the retry)", st)
	}
}
