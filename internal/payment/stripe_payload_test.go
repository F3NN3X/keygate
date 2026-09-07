package payment

import (
	"encoding/json"
	"testing"
)

// Webhook payload shapes differ by the endpoint's API version. Both
// must resolve to the same subscription and period end.
func TestInvoiceEvent_SubscriptionID_BothShapes(t *testing.T) {
	legacy := []byte(`{"id":"in_1","subscription":"sub_legacy","period_end":1700000000}`)
	basil := []byte(`{"id":"in_1","period_end":1700000000,"parent":{"type":"subscription_details","subscription_details":{"subscription":"sub_basil"}}}`)
	oneOff := []byte(`{"id":"in_1","period_end":1700000000,"parent":null}`)

	var e invoiceEvent
	if json.Unmarshal(legacy, &e) != nil || e.SubscriptionID() != "sub_legacy" {
		t.Fatalf("legacy shape: got %q", e.SubscriptionID())
	}
	e = invoiceEvent{}
	if json.Unmarshal(basil, &e) != nil || e.SubscriptionID() != "sub_basil" {
		t.Fatalf("basil shape: got %q", e.SubscriptionID())
	}
	e = invoiceEvent{}
	if json.Unmarshal(oneOff, &e) != nil || e.SubscriptionID() != "" {
		t.Fatalf("one-off invoice: got %q, want empty", e.SubscriptionID())
	}
}

func TestSubscriptionEvent_PeriodEnd_BothShapes(t *testing.T) {
	legacy := []byte(`{"id":"sub_1","status":"active","current_period_end":1700000000,"items":{"data":[{"current_period_end":1600000000}]}}`)
	basil := []byte(`{"id":"sub_1","status":"active","items":{"data":[{"current_period_end":1700000000},{"current_period_end":1700005000}]}}`)
	none := []byte(`{"id":"sub_1","status":"canceled","items":{"data":[]}}`)

	var e subscriptionEvent
	if json.Unmarshal(legacy, &e) != nil || e.PeriodEnd() != 1700000000 {
		t.Fatalf("legacy shape: got %d", e.PeriodEnd())
	}
	e = subscriptionEvent{}
	if json.Unmarshal(basil, &e) != nil || e.PeriodEnd() != 1700005000 {
		t.Fatalf("basil shape: got %d, want latest item period end", e.PeriodEnd())
	}
	e = subscriptionEvent{}
	if json.Unmarshal(none, &e) != nil || e.PeriodEnd() != 0 {
		t.Fatalf("no period: got %d, want 0", e.PeriodEnd())
	}
}
