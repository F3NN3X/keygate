package payment

import (
	"fmt"
	"net/http"
	"testing"
)

// One payment intent may be tried against several invoices, so
// the invoice a refunded charge paid is looked up among its *paid*
// invoice payments (status=paid), in both refundUpgrade's
// invoiceForCharge and licenseForCharge. The stub answers an unfiltered
// list with a failed attempt on another subscription's invoice first.
func TestChargeInvoiceIsTheOneItPaid(t *testing.T) {
	f := newUpgradeFixture(t, "paidinv")
	subID, other, pi := "sub_pd_"+f.tag, "sub_pdx_"+f.tag, "pi_pd_"+f.tag
	l := f.reload(t)
	l.StripeSubscriptionID, l.PaymentProvider = subID, "stripe"
	if err := f.s.UpdateLicense(f.ctx, l, "stripe_subscription_id", "payment_provider"); err != nil {
		t.Fatal(err)
	}
	inv := func(id, status, sub string) string {
		return fmt.Sprintf(`{"id":"inpay_%s","object":"invoice_payment","status":%q,
			"invoice":{"id":%q,"object":"invoice","billing_reason":"subscription_cycle","parent":{"type":"subscription_details","subscription_details":{"subscription":%q}}}}`, id, status, id, sub)
	}
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/invoice_payments" {
			http.Error(w, `{"error":{"message":"unexpected"}}`, http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("status") == "paid" {
			fmt.Fprintf(w, `{"object":"list","has_more":false,"url":"/v1/invoice_payments","data":[%s]}`, inv("in_paid", "paid", subID))
			return
		}
		fmt.Fprintf(w, `{"object":"list","has_more":false,"url":"/v1/invoice_payments","data":[%s,%s]}`,
			inv("in_tried", "failed", other), inv("in_paid", "paid", subID))
	})

	got, err := f.h.invoiceForCharge("", pi)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != "in_paid" {
		t.Errorf("invoiceForCharge = %v, want in_paid (the invoice the payment paid)", got)
	}

	lic, paid, err := f.h.licenseForCharge(f.ctx, "ch_pd_"+f.tag, pi, "", "", chargeInvoice{})
	if err != nil {
		t.Fatal(err)
	}
	if lic == nil || lic.ID != l.ID || paid == nil || paid.ID != "in_paid" {
		t.Errorf("licenseForCharge = %v, %v; want licence %s through in_paid", lic, paid, l.ID)
	}
}
