package payment

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stripe/stripe-go/v82"
	portalsession "github.com/stripe/stripe-go/v82/billingportal/session"
	"github.com/stripe/stripe-go/v82/checkout/session"
	stripecustomer "github.com/stripe/stripe-go/v82/customer"
	stripeinvoice "github.com/stripe/stripe-go/v82/invoice"
	"github.com/stripe/stripe-go/v82/invoicepayment"
	stripeprice "github.com/stripe/stripe-go/v82/price"
	"github.com/stripe/stripe-go/v82/subscription"
	"github.com/stripe/stripe-go/v82/webhook"

	"github.com/tabloy/keygate/internal/license"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

type StripeHandler struct {
	Store         *store.Store
	WebhookSecret string // initial value from config (backward compat)
	BaseURL       string
	Email         *service.EmailService
	WebhookSvc    *service.WebhookService
	// Livemode is the environment this handler is configured for.
	// Every inbound webhook event whose Livemode differs is rejected
	// with 400 — guards against cross-environment delivery (test
	// secret leaking + replay into prod, or vice versa).
	Livemode bool

	// pendingAfter is where the last SyncPendingCheckouts round
	// stopped when it hit its per-round cap; nil starts from the oldest
	// row. pendingBatchLimit overrides the cap (tests); 0 = default.
	pendingAfter      *store.PendingCheckoutSession
	pendingBatchLimit int

	mu            sync.RWMutex
	webhookSecret string // runtime-updatable, guarded by mu
	// prevWebhookSecret is accepted alongside webhookSecret until
	// prevSecretUntil, while a replaced endpoint drains its queue.
	prevWebhookSecret string
	prevSecretUntil   time.Time
}

// SetPreviousWebhookSecret keeps an older signing secret valid until
// `until`, so deliveries still queued for a replaced endpoint verify.
func (h *StripeHandler) SetPreviousWebhookSecret(secret string, until time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prevWebhookSecret, h.prevSecretUntil = secret, until
}

func (h *StripeHandler) previousWebhookSecret() (string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.prevWebhookSecret == "" || time.Now().After(h.prevSecretUntil) {
		return "", false
	}
	return h.prevWebhookSecret, true
}

// GetWebhookSecret returns the current webhook signing secret (thread-safe).
func (h *StripeHandler) GetWebhookSecret() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.webhookSecret
}

// SetWebhookSecret updates the webhook signing secret (thread-safe).
func (h *StripeHandler) SetWebhookSecret(secret string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.webhookSecret = secret
}

// isSameOrigin checks that a URL shares the same scheme+host as BaseURL
// and has no userinfo (to prevent https://evil.com@legit.com bypasses).
func (h *StripeHandler) isSameOrigin(raw string) bool {
	base, err := url.Parse(h.BaseURL)
	if err != nil {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Scheme == base.Scheme && u.Host == base.Host && u.User == nil
}

func (h *StripeHandler) CreateCheckoutSession(c *gin.Context) {
	var req struct {
		PriceID    string `json:"price_id" binding:"required"`
		Email      string `json:"email"`
		SuccessURL string `json:"success_url"`
		CancelURL  string `json:"cancel_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "price_id is required")
		return
	}

	plan, err := h.Store.FindPlanByStripePrice(c, req.PriceID)
	if err != nil || plan == nil {
		response.BadRequest(c, "invalid price_id")
		return
	}

	success := h.BaseURL + "/checkout/success?session_id={CHECKOUT_SESSION_ID}"
	if req.SuccessURL != "" && h.isSameOrigin(req.SuccessURL) {
		success = req.SuccessURL
	}
	cancel := h.BaseURL + "/pricing"
	if req.CancelURL != "" && h.isSameOrigin(req.CancelURL) {
		cancel = req.CancelURL
	}

	mode := string(stripe.CheckoutSessionModeSubscription)
	if plan.LicenseType == "perpetual" {
		mode = string(stripe.CheckoutSessionModePayment)
	}

	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(mode),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{Price: stripe.String(req.PriceID), Quantity: stripe.Int64(1)},
		},
		SuccessURL:          stripe.String(success),
		CancelURL:           stripe.String(cancel),
		AllowPromotionCodes: stripe.Bool(true),
	}
	params.Metadata = map[string]string{
		"plan_id":    plan.ID,
		"product_id": plan.ProductID,
	}
	if req.Email != "" {
		params.CustomerEmail = stripe.String(req.Email)
	}

	s, err := session.New(params)
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, gin.H{"url": s.URL, "session_id": s.ID})
}

// CheckoutByPlan handles GET /pay/:checkout_id — looks up plan by checkout_id,
// creates a Stripe Checkout Session, and redirects to Stripe.
func (h *StripeHandler) CheckoutByPlan(c *gin.Context) {
	checkoutID := c.Param("checkout_id")
	if len(checkoutID) != 8 {
		c.String(http.StatusBadRequest, "invalid checkout id")
		return
	}

	plan, err := h.Store.FindPlanByCheckoutID(c, checkoutID)
	if err != nil || plan == nil {
		c.String(http.StatusNotFound, "plan not found")
		return
	}

	if !plan.Active {
		c.String(http.StatusGone, "this plan is no longer available")
		return
	}

	if plan.StripePriceID == "" {
		c.String(http.StatusServiceUnavailable, "payment not configured for this plan")
		return
	}

	// Determine checkout mode from Stripe Price (source of truth, not local config)
	sp, err := stripeprice.Get(plan.StripePriceID, nil)
	if err != nil {
		slog.Error("stripe: failed to fetch price", "price_id", plan.StripePriceID, "error", err)
		c.String(http.StatusServiceUnavailable, "payment configuration error")
		return
	}
	mode := string(stripe.CheckoutSessionModeSubscription)
	if sp.Type == "one_time" {
		mode = string(stripe.CheckoutSessionModePayment)
	}

	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(mode),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{Price: stripe.String(plan.StripePriceID), Quantity: stripe.Int64(1)},
		},
		SuccessURL:          stripe.String(h.BaseURL + "/checkout/success?session_id={CHECKOUT_SESSION_ID}"),
		CancelURL:           stripe.String(h.BaseURL + "/pricing"),
		AllowPromotionCodes: stripe.Bool(true),
	}
	params.Metadata = map[string]string{
		"plan_id":    plan.ID,
		"product_id": plan.ProductID,
	}

	s, err := session.New(params)
	if err != nil {
		c.String(http.StatusInternalServerError, "checkout unavailable")
		return
	}

	c.Redirect(http.StatusTemporaryRedirect, s.URL)
}

func (h *StripeHandler) Webhook(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		response.BadRequest(c, "read failed")
		return
	}

	secret := h.GetWebhookSecret()
	if secret == "" {
		slog.Error("stripe webhook received but no signing secret configured")
		response.BadRequest(c, "webhook not configured")
		return
	}
	// Signature check here; the API version is checked below against
	// the shapes this build parses. stripe-go's own guard would refuse
	// every pre-2025 event, silently disabling billing for accounts
	// whose endpoint predates the release trains — exactly the installs
	// that set STRIPE_WEBHOOK_SECRET by hand.
	opts := webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true}
	event, err := webhook.ConstructEventWithOptions(body, c.GetHeader("Stripe-Signature"), secret, opts)
	if err != nil {
		// A replaced endpoint keeps delivering (and retrying) with its
		// old secret for a while; accept it during the drain period.
		if prev, ok := h.previousWebhookSecret(); ok {
			event, err = webhook.ConstructEventWithOptions(body, c.GetHeader("Stripe-Signature"), prev, opts)
		}
	}
	if err != nil {
		slog.Error("stripe webhook verification failed", "error", err.Error())
		response.BadRequest(c, "invalid signature")
		return
	}
	if !supportedAPIVersion(event.APIVersion) {
		// Not recorded as processed: once the endpoint is recreated on
		// a supported version (or this build upgraded) Stripe's retry
		// of the event will be applied.
		slog.Error("stripe webhook: unsupported API version, event not applied",
			"event_id", event.ID, "event_version", event.APIVersion, "sdk_version", stripe.APIVersion)
		response.Err(c, http.StatusBadRequest, "UNSUPPORTED_API_VERSION",
			"webhook endpoint API version "+event.APIVersion+" is not supported by this build; recreate the endpoint on "+stripe.APIVersion)
		return
	}

	// Livemode gate: even with a valid signature, a test-mode event
	// must not be processed by a live server (or vice versa). Stripe
	// allows separate webhook endpoints per mode, so the secret alone
	// doesn't carry environment info — we check the event flag
	// against our configured mode. Without this, a leaked test secret
	// could replay arbitrary forged events at the production endpoint.
	ctx := c.Request.Context()
	if event.Livemode != h.Livemode {
		slog.Error("stripe webhook livemode mismatch",
			"event_id", event.ID, "event_livemode", event.Livemode,
			"server_livemode", h.Livemode)
		response.BadRequest(c, "livemode mismatch")
		return
	}

	// Idempotency. A claim is taken before handling and a done marker
	// written after; a claim without a done marker that is older than
	// staleClaimAge is taken over, so an event whose handling failed
	// (and whose claim could not be released) is not mistaken for a
	// completed one when Stripe retries it.
	claimed, done, err := h.claimEvent(ctx, event.ID)
	if err != nil {
		slog.Error("stripe webhook: failed to claim event", "id", event.ID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"received": false, "retry": true})
		return
	}
	if !claimed {
		if done {
			c.JSON(http.StatusOK, gin.H{"received": true, "skipped": true})
			return
		}
		// Another delivery of this event is being handled right now
		// (or failed moments ago and could not release its claim). A
		// 2xx would end Stripe's retries; ask it to come back instead.
		c.JSON(http.StatusServiceUnavailable, gin.H{"received": false, "retry": true, "in_progress": true})
		return
	}

	slog.Info("stripe webhook received", "type", event.Type, "id", event.ID)

	// Handlers that talk to Stripe report transient failures; the
	// event claim is then released and Stripe asked to retry, so a
	// lookup that failed once does not mean a paid customer without a
	// license or a refunded one that stays active.
	var herr error
	switch event.Type {
	case "checkout.session.completed", "checkout.session.async_payment_succeeded":
		herr = h.onCheckoutCompleted(ctx, event.Data.Raw)
	case "checkout.session.async_payment_failed":
		h.onAsyncPaymentFailed(ctx, event.Data.Raw)
	case "invoice.paid":
		h.onInvoicePaid(ctx, event.Data.Raw)
	case "customer.subscription.updated":
		h.onSubscriptionUpdated(ctx, event.Data.Raw)
	case "customer.subscription.deleted":
		h.onSubscriptionDeleted(ctx, event.Data.Raw)
	case "invoice.payment_failed":
		h.onPaymentFailed(ctx, event.Data.Raw)
	case "charge.refunded":
		herr = h.onChargeRefunded(ctx, event.Data.Raw)
	case "charge.dispute.created":
		h.onDisputeCreated(ctx, event.Data.Raw)
	case "charge.dispute.closed":
		h.onDisputeClosed(ctx, event.Data.Raw)
	case "invoice.payment_action_required":
		h.onPaymentActionRequired(ctx, event.Data.Raw)
	case "customer.subscription.paused":
		h.onSubscriptionPaused(ctx, event.Data.Raw)
	case "customer.subscription.resumed":
		h.onSubscriptionResumed(ctx, event.Data.Raw)
	case "customer.subscription.trial_will_end":
		h.onTrialWillEnd(ctx, event.Data.Raw)
	case "invoice.upcoming":
		h.onInvoiceUpcoming(ctx, event.Data.Raw)
	case "customer.updated":
		h.onCustomerUpdated(ctx, event.Data.Raw)
	default:
		slog.Warn("stripe webhook: unhandled event type", "type", event.Type)
	}
	if herr != nil {
		slog.Warn("stripe webhook: transient failure, asking Stripe to retry", "type", event.Type, "id", event.ID, "error", herr)
		// Best effort: if this fails the claim goes stale and the
		// retry takes it over once it is old enough.
		if err := h.release(ctx, processedEventDoneProvider, processedEventClaimProvider, event.ID); err != nil {
			slog.Error("stripe webhook: failed to release event claim", "id", event.ID, "error", err)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"received": false, "retry": true})
		return
	}
	// Dropping the in-flight marker is what makes a later resend skip.
	// If that fails, do not tell Stripe the event is delivered: its
	// retry finds the marker and comes back until it is gone.
	if err := h.Store.CompleteProcessedEvent(ctx, processedEventClaimProvider, event.ID); err != nil {
		slog.Error("stripe webhook: failed to record completion", "id", event.ID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"received": false, "retry": true})
		return
	}

	c.JSON(http.StatusOK, gin.H{"received": true})
}

// Stripe events use two rows while a delivery is in flight and one
// once it is done:
//
//   - the reservation, under the provider name every binary so far has
//     used ("stripe"). Older binaries during a rolling upgrade see it
//     as their own idempotency row and skip the event; rows they write
//     read as done here, never as stale claims.
//   - the in-flight marker ("stripe_claim"), removed on completion.
//     While it exists the event is being handled (or its handler died:
//     after staleClaimAge it is taken over).
const (
	processedEventClaimProvider = "stripe_claim"
	processedEventDoneProvider  = "stripe"
)

// claimEvent takes the processing claim for a Stripe event. claimed
// is false when the event is done (done=true) or another handler
// holds a fresh claim (done=false); a stale claim without a done
// marker is taken over.
func (h *StripeHandler) claimEvent(ctx context.Context, eventID string) (claimed, done bool, err error) {
	return h.reserve(ctx, processedEventDoneProvider, processedEventClaimProvider, eventID, nil)
}

// reserve implements the two-row claim for events and sessions.
// claimed: this caller now handles it. done: nothing to do (finished,
// or an older binary has it). Neither: another caller is mid-way, or
// (with err) the state could not be established. fulfilled, when
// given, is an extra "already done" check — a license row for a
// session — consulted before taking over a stale in-flight marker.
func (h *StripeHandler) reserve(ctx context.Context, doneProvider, claimProvider, id string, fulfilled func() (bool, error)) (claimed, done bool, err error) {
	reserved, err := h.Store.ClaimProcessedEvent(ctx, doneProvider, id)
	if err != nil {
		return false, false, err
	}
	if reserved {
		// Ours. An in-flight marker without a reservation can only be
		// an orphan of a failed release; replace it.
		_ = h.Store.DeleteProcessedEvent(ctx, claimProvider, id)
		claimed, err = h.Store.ClaimProcessedEvent(ctx, claimProvider, id)
		if err != nil || !claimed {
			_ = h.Store.DeleteProcessedEvent(ctx, doneProvider, id)
			return false, false, err
		}
		return true, false, nil
	}
	inflight, err := h.Store.HasProcessedEvent(ctx, claimProvider, id)
	if err != nil {
		return false, false, err
	}
	if !inflight {
		return false, true, nil // reservation without marker: done
	}
	if fulfilled != nil {
		if ok, err := fulfilled(); err != nil || ok {
			return false, ok, err
		}
	}
	released, err := h.Store.ReleaseStaleProcessedEvent(ctx, claimProvider, id, staleClaimAge)
	if err != nil || !released {
		return false, false, err // fresh: someone is on it
	}
	slog.Warn("stripe: took over a stale in-flight claim", "provider", claimProvider, "id", id)
	claimed, err = h.Store.ClaimProcessedEvent(ctx, claimProvider, id)
	return claimed, false, err
}

// release undoes a reservation after a failed handling so a retry
// can claim it. The reservation goes first: if only the marker were
// removed the event would read as done and never be retried.
func (h *StripeHandler) release(ctx context.Context, doneProvider, claimProvider, id string) error {
	if err := h.Store.DeleteProcessedEvent(ctx, doneProvider, id); err != nil {
		return err
	}
	return h.Store.DeleteProcessedEvent(ctx, claimProvider, id)
}

// onCheckoutCompleted handles checkout.session.completed and
// checkout.session.async_payment_succeeded. Delayed payment methods
// (bank debits, vouchers) complete the session before the money
// arrives; the session is then still unpaid and must not produce a
// license. Nothing is claimed for it, so the later
// async_payment_succeeded event fulfils it through this same path.
func (h *StripeHandler) onCheckoutCompleted(ctx context.Context, raw json.RawMessage) error {
	var data struct {
		ID              string `json:"id"`
		CustomerEmail   string `json:"customer_email"`
		CustomerDetails *struct {
			Email string `json:"email"`
		} `json:"customer_details"`
		Customer      string            `json:"customer"`
		Subscription  string            `json:"subscription"`
		PaymentIntent string            `json:"payment_intent"`
		PaymentStatus string            `json:"payment_status"`
		Mode          string            `json:"mode"`
		Metadata      map[string]string `json:"metadata"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return nil
	}
	if !sessionPaid(data.PaymentStatus) {
		// Remember it: if async_payment_succeeded is lost, the
		// periodic SyncPendingCheckouts still picks the session up.
		// That marker is the whole point of this branch, so failing
		// to write it fails the event.
		if _, err := h.Store.ClaimProcessedEvent(ctx, pendingSessionProvider, data.ID); err != nil {
			return fmt.Errorf("record pending session %s: %w", data.ID, err)
		}
		slog.Info("stripe checkout: session not paid yet, waiting", "session_id", data.ID, "payment_status", data.PaymentStatus)
		return nil
	}
	email := data.CustomerEmail
	if email == "" && data.CustomerDetails != nil {
		email = data.CustomerDetails.Email
	}
	if data.Metadata == nil {
		data.Metadata = map[string]string{}
	}
	data.Metadata["session_id"] = data.ID
	ok, err := h.fulfillCheckout(ctx, email, data.Customer, data.Subscription, data.PaymentIntent, data.Metadata, "webhook")
	if !ok {
		// Not fulfilled here — a transient failure, another worker
		// mid-way, or a plan/email the operator still has to fix. The
		// pending marker keeps the session in SyncPendingCheckouts for
		// 30 days; on an error Stripe also retries meanwhile.
		if _, perr := h.Store.ClaimProcessedEvent(ctx, pendingSessionProvider, data.ID); perr != nil {
			return fmt.Errorf("record pending session %s: %w", data.ID, perr)
		}
	}
	return err
}

// supportedAPIVersion reports whether this build parses events
// rendered at the given Stripe API version. The handlers read two
// layouts: the pre-2025-03-31 one (dotless date versions and the
// Acacia train) and the Basil one the SDK is pinned to. Any other
// train may carry fields in places the parsers do not look, and must
// not be acknowledged as applied.
func supportedAPIVersion(v string) bool {
	if v == "" {
		return false
	}
	i := strings.Index(v, ".")
	if i < 0 {
		return true // yyyy-MM-dd: legacy layout
	}
	switch v[i+1:] {
	case "acacia":
		return true // last pre-Basil train, legacy layout
	}
	return sameReleaseTrain(v, stripe.APIVersion)
}

// pendingSessionProvider keys processed_events rows for sessions that
// completed before their delayed payment settled.
const pendingSessionProvider = "stripe_pending_session"

func (h *StripeHandler) onAsyncPaymentFailed(ctx context.Context, raw json.RawMessage) {
	var data struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &data) != nil || data.ID == "" {
		return
	}
	_ = h.Store.DeleteProcessedEvent(ctx, pendingSessionProvider, data.ID)
	slog.Info("stripe checkout: delayed payment failed, session dropped", "session_id", data.ID)
}

func sessionPaid(status string) bool {
	return status == "paid" || status == "no_payment_required"
}

// sessionEmail prefers the email Stripe attached to the session and
// falls back to the address typed during Checkout — guest checkouts
// on Payment Links carry only the latter.
func sessionEmail(sess *stripe.CheckoutSession) string {
	if sess.CustomerEmail != "" {
		return sess.CustomerEmail
	}
	if sess.CustomerDetails != nil {
		return sess.CustomerDetails.Email
	}
	return ""
}

// fulfillCheckout creates a license for a completed checkout session
// and reports whether the session is fulfilled (now or earlier).
// Idempotent per session: the session ID is claimed atomically right
// before the license is written, so the webhook, success-page
// verification and periodic sync can all see the same session and
// only one license comes out. A customer who completes a second
// checkout for the same product gets a second license — one paid
// session, one license.
//
// The bool says whether the session is fulfilled (now or earlier). A
// non-nil error is a transient failure (Stripe or database) — the
// caller should retry later; permanent conditions (unknown plan, no
// email) return false with no error.
func (h *StripeHandler) fulfillCheckout(ctx context.Context, email, customerID, subscriptionID, paymentIntentID string, metadata map[string]string, source string) (bool, error) {
	sessionID := ""
	if metadata != nil {
		sessionID = metadata["session_id"]
	}
	if sessionID != "" {
		done, err := h.sessionFulfilled(ctx, sessionID)
		if err != nil {
			return false, fmt.Errorf("check session: %w", err)
		}
		if done {
			return true, nil
		}
	}
	var plan *model.Plan
	var err error

	if subscriptionID != "" {
		if plan, err = h.resolvePlan(ctx, subscriptionID); err != nil {
			return false, fmt.Errorf("resolve plan from subscription: %w", err)
		}
	}
	if plan == nil && metadata != nil && metadata["plan_id"] != "" {
		// FindPlanByID hands back a non-nil empty plan together with
		// the error; a deleted plan must read as "no plan", a database
		// failure as transient — never as an empty plan to insert.
		switch p, err := h.Store.FindPlanByID(ctx, metadata["plan_id"]); {
		case err == nil:
			plan = p
		case !errors.Is(err, sql.ErrNoRows):
			return false, fmt.Errorf("find plan %s: %w", metadata["plan_id"], err)
		}
	}
	// Sessions created outside Keygate (Stripe Payment Links, the
	// merchant's own integration) carry no plan_id metadata, and a
	// one-time payment has no subscription to look up. The line items
	// still say which price was bought — resolve the plan from that.
	if plan == nil && sessionID != "" {
		if plan, err = h.resolvePlanFromLineItems(ctx, sessionID); err != nil {
			return false, fmt.Errorf("resolve plan from line items: %w", err)
		}
	}
	if plan == nil {
		slog.Warn("stripe checkout: could not resolve plan", "subscription_id", subscriptionID, "metadata", metadata, "source", source)
		return false, nil
	}

	// Resolve email from Stripe Customer (authoritative source)
	if customerID != "" {
		cust, err := stripecustomer.Get(customerID, nil)
		switch {
		case err == nil && cust.Email != "":
			email = cust.Email
		case err != nil && !stripeNotFound(err):
			return false, fmt.Errorf("fetch customer: %w", err)
		}
	}

	if email == "" {
		slog.Warn("stripe checkout: no customer email, skipping", "customer_id", customerID, "source", source)
		return false, nil
	}

	// Claim the session only now, after every Stripe lookup succeeded:
	// a transient API error above must leave the session unclaimed so
	// the success page or the periodic sync can pick it up. The insert
	// is atomic, so concurrent callers still produce a single license.
	if sessionID != "" {
		claimed, err := h.claimSession(ctx, sessionID)
		if err != nil {
			return false, fmt.Errorf("claim session: %w", err)
		}
		if !claimed {
			// Fulfilled meanwhile, or claimed by a caller still
			// running. Only the former is a completed fulfilment.
			done, err := h.sessionFulfilled(ctx, sessionID)
			if err != nil {
				return false, fmt.Errorf("check session: %w", err)
			}
			return done, nil
		}
	}

	status := model.StatusActive
	if plan.LicenseType == "trial" {
		status = model.StatusTrialing
	}

	lic := &model.License{
		ProductID:        plan.ProductID,
		PlanID:           plan.ID,
		Email:            email,
		LicenseKey:       license.GenerateKey(""),
		PaymentProvider:  "stripe",
		StripeCustomerID: customerID,
		Status:           status,
	}

	if plan.LicenseType == "trial" && plan.TrialDays > 0 {
		until := time.Now().Add(time.Duration(plan.TrialDays) * 24 * time.Hour)
		lic.ValidUntil = &until
	}

	if subscriptionID != "" {
		lic.StripeSubscriptionID = subscriptionID
	}
	lic.StripePaymentIntentID = paymentIntentID
	lic.StripeCheckoutSessionID = sessionID

	// Ensure user record exists so they appear in Customers
	_ = h.Store.UpsertUser(ctx, &model.User{Email: email})

	if err := h.Store.CreateLicenseWithSubscription(ctx, lic, plan); err != nil {
		if store.IsCheckoutSessionConflict(err) {
			// Another worker fulfilled this session while we held (or
			// had lost) the claim — its license stands, ours is refused
			// by the unique index. Nothing to release.
			slog.Warn("stripe checkout: session fulfilled concurrently", "session_id", sessionID)
			return true, nil
		}
		slog.Error("stripe checkout: failed to create license", "email", email, "error", err)
		// Release the claim: the customer has paid, and a later
		// webhook retry, success-page visit or sync must be able to
		// try again instead of short-circuiting on the marker.
		if sessionID != "" {
			if derr := h.release(ctx, fulfilledSessionProvider, sessionClaimProvider, sessionID); derr != nil {
				slog.Error("stripe checkout: failed to release session claim", "session_id", sessionID, "error", derr)
			}
		}
		return false, fmt.Errorf("create license: %w", err)
	}
	if sessionID != "" {
		// The reservation stays as the durable done marker; the
		// in-flight marker goes. Failing here is not fatal: the license
		// (with its session id) already answers every retry.
		if err := h.Store.CompleteProcessedEvent(ctx, sessionClaimProvider, sessionID); err != nil {
			slog.Error("stripe checkout: failed to record session completion", "session_id", sessionID, "error", err)
		}
		// The session is no longer pending; a marker left by an
		// earlier unpaid completion or a racing worker goes away.
		_ = h.Store.DeleteProcessedEvent(ctx, pendingSessionProvider, sessionID)
	}

	// Link license to user
	if u, err := h.Store.FindUserByEmail(ctx, email); err == nil {
		lic.UserID = u.ID
		_ = h.Store.UpdateLicenseUser(ctx, lic.ID, u.ID)
	}

	productName := h.productName(ctx, plan.ProductID)
	if email != "" {
		// Use DecryptLicenseKey for forward compatibility — Phase C will
		// drop the plaintext column and direct .LicenseKey reads will be empty.
		displayKey := h.Store.DecryptLicenseKey(lic)
		// Render through the shared template so a customised
		// email_template_license_created setting reaches buyers, not only
		// admin-created licences. h.Email is nil in unit tests, which have no
		// email service wired; fall back to the built-in body there.
		subject := "Your license for " + productName
		var body string
		if h.Email != nil {
			subject, body = h.Email.RenderLicenseCreated(productName, plan.Name, displayKey)
		} else {
			body = fmt.Sprintf(`<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #111;">Your %s License</h2>
<p>Your <strong>%s</strong> license is ready.</p>
<div style="background: #f4f4f5; border-radius: 8px; padding: 16px; margin: 16px 0; font-family: monospace; font-size: 18px; text-align: center; letter-spacing: 2px;">%s</div>
<p style="color: #666; font-size: 14px;">Keep this key safe. You'll need it to activate your software.</p>
</body></html>`, productName, plan.Name, displayKey)
		}
		_ = h.Store.EnqueueEmail(ctx, email, subject, body)
	}

	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "created",
		ActorType: source,
		Changes:   map[string]any{"provider": "stripe", "email": email, "plan": plan.Name},
	})

	if h.WebhookSvc != nil {
		h.WebhookSvc.Dispatch(ctx, lic.ProductID, "license.created", map[string]any{
			"license_id": lic.ID, "email": lic.Email, "plan_id": lic.PlanID,
		})
	}

	slog.Info("license created", "email", email, "plan", plan.Name, "source", source)
	return true, nil
}

// VerifyCheckoutSession handles GET /api/v1/checkout/verify?session_id=xxx
// Called by the success page to verify payment and create license (webhook fallback).
func (h *StripeHandler) VerifyCheckoutSession(c *gin.Context) {
	sessionID := c.Query("session_id")
	if sessionID == "" {
		response.BadRequest(c, "session_id is required")
		return
	}

	sess, err := session.Get(sessionID, &stripe.CheckoutSessionParams{})
	if err != nil {
		response.BadRequest(c, "invalid session")
		return
	}

	if !sessionPaid(string(sess.PaymentStatus)) {
		response.OK(c, gin.H{"status": "pending"})
		return
	}

	ok, err := h.fulfillSession(c.Request.Context(), sess, "verify")
	if !ok {
		h.Store.TryRecordProcessedEvent(c, pendingSessionProvider, sess.ID)
	}
	if err != nil {
		slog.Warn("stripe verify: transient fulfilment failure", "session_id", sess.ID, "error", err)
		response.Err(c, http.StatusServiceUnavailable, "FULFILLMENT_RETRY", "payment confirmed; license delivery is being retried")
		return
	}
	if !ok {
		// Paid, but no license yet (another worker is on it, or the
		// plan mapping needs attention); the sync keeps trying.
		response.OK(c, gin.H{"status": "pending", "email": sessionEmail(sess)})
		return
	}

	response.OK(c, gin.H{"status": "ok", "email": sessionEmail(sess)})
}

// sessionFulfilled reports whether a license already exists for the
// checkout session. A claim row alone is not proof: its owner may
// still be running, or may have failed and released it.
func (h *StripeHandler) sessionFulfilled(ctx context.Context, sessionID string) (bool, error) {
	_, err := h.Store.FindLicenseByStripeCheckoutSession(ctx, sessionID)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	reserved, err := h.Store.HasProcessedEvent(ctx, fulfilledSessionProvider, sessionID)
	if err != nil || !reserved {
		return false, err
	}
	inflight, err := h.Store.HasProcessedEvent(ctx, sessionClaimProvider, sessionID)
	return !inflight, err
}

// Checkout sessions follow the same two-row scheme as events: the
// reservation under the name every binary so far has used
// ("stripe_fulfill"), plus an in-flight marker until the license is
// committed. Sessions older binaries fulfilled — before the upgrade or
// while overlapping it — carry only the reservation and read as done;
// their licenses have no session id, so that row is their record.
const (
	sessionClaimProvider     = "stripe_fulfill_claim"
	fulfilledSessionProvider = "stripe_fulfill"
)

// stripeNotFound reports a 404 from Stripe: the object is gone for
// good, which is a permanent condition, not a transient failure.
func stripeNotFound(err error) bool {
	var serr *stripe.Error
	return errors.As(err, &serr) && serr.HTTPStatusCode == http.StatusNotFound
}

// staleClaimAge bounds how long a fulfilment claim may sit without a
// license before another caller takes it over — long enough for the
// slowest legitimate fulfilment (a few Stripe calls), short enough
// that a crashed process does not strand a paid session.
const staleClaimAge = 10 * time.Minute

// claimSession takes the fulfilment claim for a session. A claim
// left behind by a caller that died mid-way is released after
// staleClaimAge; a claim released by a failed insert is simply gone.
//
// A database error is returned as such: the caller must treat the
// session as not fulfilled and keep it scheduled for retry.
func (h *StripeHandler) claimSession(ctx context.Context, sessionID string) (bool, error) {
	claimed, _, err := h.reserve(ctx, fulfilledSessionProvider, sessionClaimProvider, sessionID, func() (bool, error) {
		_, err := h.Store.FindLicenseByStripeCheckoutSession(ctx, sessionID)
		if err == nil {
			return true, nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	})
	return claimed, err
}

// fulfillSession fulfils a session fetched from the Stripe API.
func (h *StripeHandler) fulfillSession(ctx context.Context, sess *stripe.CheckoutSession, source string) (bool, error) {
	custID := ""
	if sess.Customer != nil {
		custID = sess.Customer.ID
	}
	subID := ""
	if sess.Subscription != nil {
		subID = sess.Subscription.ID
	}
	meta := sess.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	meta["session_id"] = sess.ID
	return h.fulfillCheckout(ctx, sessionEmail(sess), custID, subID, paymentIntentID(sess), meta, source)
}

// SyncPendingCheckouts re-checks sessions that completed before their
// delayed payment (bank debit, voucher) settled. Those can pay hours
// or days later, outside the window SyncRecentCheckouts scans, so a
// lost async_payment_succeeded event would otherwise leave a paying
// customer without a license. Only sessions this server saw complete
// unpaid are polled; each is dropped once fulfilled or expired.
func (h *StripeHandler) SyncPendingCheckouts(ctx context.Context) {
	pageSize, maxPerRound := 100, 1000
	if h.pendingBatchLimit > 0 {
		maxPerRound = h.pendingBatchLimit
		if pageSize > maxPerRound {
			pageSize = maxPerRound
		}
	}
	// A worker that lost the race records a pending marker after the
	// winner already cleared it; sweep those so the backlog only holds
	// sessions that still need work.
	if err := h.Store.DeleteFulfilledPendingSessions(ctx); err != nil {
		slog.Warn("stripe sync: failed to sweep fulfilled pending sessions", "error", err)
	}
	// Continue where the previous round stopped, so a backlog larger
	// than one round is walked to the end across rounds before the
	// walk starts over; nothing waits behind the oldest thousand.
	after := h.pendingAfter
	for seen := 0; seen < maxPerRound; {
		rows, err := h.Store.ListPendingCheckoutSessions(ctx, 30*24*time.Hour, after, pageSize)
		if err != nil {
			slog.Error("stripe sync: failed to list pending sessions", "error", err)
			return
		}
		if len(rows) == 0 {
			h.pendingAfter = nil // walked to the end; next round starts over
			return
		}
		for i := range rows {
			h.syncPendingSession(ctx, rows[i].SessionID)
		}
		seen += len(rows)
		after = &rows[len(rows)-1]
	}
	h.pendingAfter = after
}

func (h *StripeHandler) syncPendingSession(ctx context.Context, id string) {
	{
		sess, err := session.Get(id, nil)
		if err != nil {
			slog.Warn("stripe sync: failed to fetch pending session", "session_id", id, "error", err)
			return
		}
		switch {
		case sessionPaid(string(sess.PaymentStatus)):
			if ok, err := h.fulfillSession(ctx, sess, "sync"); !ok || err != nil {
				return // try again next round
			}
		case sess.Status == stripe.CheckoutSessionStatusExpired:
		default:
			return
		}
		_ = h.Store.DeleteProcessedEvent(ctx, pendingSessionProvider, id)
	}
}

func paymentIntentID(sess *stripe.CheckoutSession) string {
	if sess == nil || sess.PaymentIntent == nil {
		return ""
	}
	return sess.PaymentIntent.ID
}

// SyncRecentCheckouts scans Stripe for completed checkout sessions in the last
// interval and creates licenses for any that were missed by webhooks.
func (h *StripeHandler) SyncRecentCheckouts(ctx context.Context) {
	// List checkout sessions completed in the last 10 minutes
	cutoff := time.Now().Add(-10 * time.Minute).Unix()
	params := &stripe.CheckoutSessionListParams{
		Status: stripe.String("complete"),
	}
	params.Filters.AddFilter("created", "gte", fmt.Sprintf("%d", cutoff))
	params.Filters.AddFilter("limit", "", "50")

	iter := session.List(params)
	for iter.Next() {
		sess := iter.CheckoutSession()
		if !sessionPaid(string(sess.PaymentStatus)) {
			// Completed on a delayed payment method and the completed
			// webhook was missed too: track it like the webhook would.
			h.Store.TryRecordProcessedEvent(ctx, pendingSessionProvider, sess.ID)
			continue
		}

		if ok, _ := h.fulfillSession(ctx, sess, "sync"); !ok {
			// Outside the creation window the pending marker is the
			// only way back to this session.
			h.Store.TryRecordProcessedEvent(ctx, pendingSessionProvider, sess.ID)
		}
	}
	if err := iter.Err(); err != nil {
		slog.Error("stripe sync: failed to list sessions", "error", err)
	}
}

// invoiceEvent is the part of an invoice webhook payload Keygate acts
// on. Stripe moved the subscription reference in API version
// 2025-03-31: older versions put it at invoice.subscription, current
// ones under invoice.parent.subscription_details.subscription. Both
// shapes arrive in practice, since a webhook endpoint keeps the API
// version it was created with.
type invoiceEvent struct {
	Subscription     string `json:"subscription"`
	PeriodEnd        int64  `json:"period_end"`
	Customer         string `json:"customer"`
	AmountDue        int64  `json:"amount_due"`
	Currency         string `json:"currency"`
	HostedInvoiceURL string `json:"hosted_invoice_url"`
	Parent           *struct {
		SubscriptionDetails *struct {
			Subscription string `json:"subscription"`
		} `json:"subscription_details"`
	} `json:"parent"`
}

// SubscriptionID returns the subscription the invoice belongs to, in
// either payload shape, or "" for one-off invoices.
func (e *invoiceEvent) SubscriptionID() string {
	if e.Subscription != "" {
		return e.Subscription
	}
	if e.Parent != nil && e.Parent.SubscriptionDetails != nil {
		return e.Parent.SubscriptionDetails.Subscription
	}
	return ""
}

// subscriptionEvent is the part of a subscription webhook payload
// Keygate acts on. current_period_end moved from the subscription to
// its items in API version 2025-03-31; read both.
type subscriptionEvent struct {
	ID               string `json:"id"`
	Status           string `json:"status"`
	CurrentPeriodEnd int64  `json:"current_period_end"`
	TrialEnd         int64  `json:"trial_end"`
	Items            struct {
		Data []struct {
			CurrentPeriodEnd int64 `json:"current_period_end"`
		} `json:"data"`
	} `json:"items"`
}

// PeriodEnd returns the end of the current billing period: the
// subscription-level value when present, else the latest item period
// end. Zero when neither is set — callers must not write that.
func (e *subscriptionEvent) PeriodEnd() int64 {
	if e.CurrentPeriodEnd > 0 {
		return e.CurrentPeriodEnd
	}
	var end int64
	for _, it := range e.Items.Data {
		if it.CurrentPeriodEnd > end {
			end = it.CurrentPeriodEnd
		}
	}
	return end
}

func (h *StripeHandler) onInvoicePaid(ctx context.Context, raw json.RawMessage) {
	var data invoiceEvent
	if json.Unmarshal(raw, &data) != nil || data.SubscriptionID() == "" {
		return
	}

	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.SubscriptionID())
	if err != nil {
		return
	}
	wasPastDue := lic.Status == model.StatusPastDue
	// Capture the episode anchor BEFORE the write clears it.
	// Without this, notifyPaymentRecovered would always see a nil
	// PastDueAt and fall back to a license-wide tag — meaning the
	// second past_due → recovery cycle ever silently drops the email.
	var episode int64
	if lic.PastDueAt != nil {
		episode = lic.PastDueAt.Unix()
	}
	until := time.Unix(data.PeriodEnd, 0)
	lic.ValidUntil = &until
	lic.Status = model.StatusActive
	lic.PastDueAt = nil
	_ = h.Store.UpdateLicenseAndSubscription(ctx, lic, "valid_until", "status", "past_due_at")

	// Recovery notification — shares the dedup path with
	// onSubscriptionUpdated. Some flows emit invoice.paid without a
	// matching subscription.updated, others emit both; both call
	// this helper which fires at most once per cycle (per episode).
	if wasPastDue {
		h.notifyPaymentRecovered(ctx, lic, episode)
	}
}

func (h *StripeHandler) onSubscriptionUpdated(ctx context.Context, raw json.RawMessage) {
	var data subscriptionEvent
	if json.Unmarshal(raw, &data) != nil {
		return
	}

	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.ID)
	if err != nil {
		return
	}

	// Capture the prior state BEFORE mutating — recovery side-effects
	// (clearing past_due_at, firing the recovered email) only run when
	// the transition is actually past_due → active.
	wasPastDue := lic.Status == model.StatusPastDue
	// Anchor for the episode-scoped recovered notification tag.
	// Captured here because the write below nulls past_due_at.
	var episode int64
	if lic.PastDueAt != nil {
		episode = lic.PastDueAt.Unix()
	}
	cols := []string{"status", "valid_until", "canceled_at", "past_due_at"}

	switch data.Status {
	case "active":
		lic.Status = model.StatusActive
		// Recovery: customer fixed the card. Clear the dunning
		// anchor so a fresh past_due episode in the future starts
		// the ladder from day 0, not from the original failure.
		lic.PastDueAt = nil
	case "past_due":
		// Idempotent entry: only stamp past_due_at on first entry
		// (or when re-entering after a recovery). Without this a
		// burst of repeated payment_failed webhooks would reset the
		// clock each time and the day-7 / day-14 reminders would
		// keep getting pushed out.
		lic.Status = model.StatusPastDue
		if lic.PastDueAt == nil {
			now := time.Now()
			lic.PastDueAt = &now
		}
	case "trialing":
		lic.Status = model.StatusTrialing
	case "canceled", "unpaid":
		lic.Status = model.StatusCanceled
		now := time.Now()
		lic.CanceledAt = &now
		lic.PastDueAt = nil
	}

	// Only move valid_until when the payload carries a period end;
	// writing the zero value would expire the license on the spot.
	if end := data.PeriodEnd(); end > 0 {
		until := time.Unix(end, 0)
		lic.ValidUntil = &until
	} else {
		cols = []string{"status", "canceled_at", "past_due_at"}
	}
	_ = h.Store.UpdateLicenseAndSubscription(ctx, lic, cols...)

	// Recovery notification fires only on past_due → active. Routed
	// through notifyPaymentRecovered so concurrent webhooks (Stripe
	// sometimes sends invoice.paid + customer.subscription.updated
	// in parallel) collapse into one email + one webhook dispatch.
	if wasPastDue && lic.Status == model.StatusActive {
		h.notifyPaymentRecovered(ctx, lic, episode)
	}
}

// notifyPaymentRecovered fires the recovery email + webhook exactly
// once per past_due episode.
//
// `episode` is the past_due_at Unix epoch captured BEFORE the
// recovery write clears the column. It enters the notification tag
// so a license that lapses → recovers → lapses → recovers in the
// same year gets the email twice (once per episode), instead of
// being permanently silenced after the first recovery by the
// notifications (license_id, tag) UNIQUE constraint.
//
// episode == 0 is a defensive fallback for legacy rows that
// somehow lack past_due_at; we still dedupe on the bare tag in
// that case to avoid a double-send within the single missing
// episode.
func (h *StripeHandler) notifyPaymentRecovered(ctx context.Context, lic *model.License, episode int64) {
	tag := "payment_recovered"
	if episode > 0 {
		tag = fmt.Sprintf("payment_recovered:%d", episode)
	}
	productName := ""
	if p, err := h.Store.FindProductByID(ctx, lic.ProductID); err == nil {
		productName = p.Name
	}
	if h.Store.HasNotification(ctx, lic.ID, tag) {
		return
	}
	if h.Email != nil {
		h.Email.SendPaymentRecovered(lic.Email, productName)
	}
	h.Store.RecordNotification(ctx, lic.ID, tag)
	if h.WebhookSvc != nil {
		h.WebhookSvc.Dispatch(ctx, lic.ProductID, "license.payment_recovered", map[string]any{
			"license_id": lic.ID, "email": lic.Email,
		})
	}
}

func (h *StripeHandler) onSubscriptionDeleted(ctx context.Context, raw json.RawMessage) {
	var data struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return
	}

	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.ID)
	if err != nil {
		return
	}
	lic.Status = model.StatusCanceled
	now := time.Now()
	lic.CanceledAt = &now
	lic.PastDueAt = nil
	_ = h.Store.UpdateLicenseAndSubscription(ctx, lic, "status", "canceled_at", "past_due_at")

	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "canceled",
		ActorType: "webhook", Changes: map[string]any{"provider": "stripe"},
	})

	if h.WebhookSvc != nil {
		h.WebhookSvc.Dispatch(ctx, lic.ProductID, "license.canceled", map[string]any{
			"license_id": lic.ID, "email": lic.Email, "reason": "subscription_deleted",
		})
	}
}

func (h *StripeHandler) onPaymentFailed(ctx context.Context, raw json.RawMessage) {
	var data invoiceEvent
	if json.Unmarshal(raw, &data) != nil || data.SubscriptionID() == "" {
		return
	}

	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.SubscriptionID())
	if err != nil {
		return
	}

	if lic.Status == model.StatusActive {
		now := time.Now()
		lic.Status = model.StatusPastDue
		lic.PastDueAt = &now
		_ = h.Store.UpdateLicenseAndSubscription(ctx, lic, "status", "past_due_at")

		h.Store.Audit(ctx, &model.AuditLog{
			Entity: "license", EntityID: lic.ID, Action: "payment_failed",
			ActorType: "webhook", Changes: map[string]any{"provider": "stripe"},
		})

		if h.WebhookSvc != nil {
			h.WebhookSvc.Dispatch(ctx, lic.ProductID, "license.payment_failed", map[string]any{
				"license_id": lic.ID, "email": lic.Email,
			})
		}
	}
}

func (h *StripeHandler) onChargeRefunded(ctx context.Context, raw json.RawMessage) error {
	var data struct {
		ID             string `json:"id"`
		Customer       string `json:"customer"`
		Amount         int64  `json:"amount"`
		AmountRefunded int64  `json:"amount_refunded"`
		Refunded       bool   `json:"refunded"`
		PaymentIntent  string `json:"payment_intent"`
		Invoice        string `json:"invoice"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return nil
	}

	lic, err := h.licenseForCharge(ctx, data.ID, data.PaymentIntent, data.Invoice, data.Customer)
	if err != nil {
		return fmt.Errorf("locate license for charge %s: %w", data.ID, err)
	}
	if lic == nil {
		return nil
	}

	if data.Refunded {
		if lic.Status == model.StatusRevoked {
			return nil // a second refund event for the same purchase; nothing more to take away
		}
		lic.Status = model.StatusRevoked
		if err := h.Store.UpdateLicenseAndSubscription(ctx, lic, "status"); err != nil {
			return fmt.Errorf("revoke license %s: %w", lic.ID, err)
		}

		h.Store.Audit(ctx, &model.AuditLog{
			Entity: "license", EntityID: lic.ID, Action: "revoked",
			ActorType: "webhook",
			Changes:   map[string]any{"reason": "full_refund", "provider": "stripe", "charge_id": data.ID},
		})
	} else if data.AmountRefunded > 0 {
		h.Store.Audit(ctx, &model.AuditLog{
			Entity: "license", EntityID: lic.ID, Action: "partial_refund",
			ActorType: "webhook",
			Changes:   map[string]any{"amount_refunded": data.AmountRefunded, "provider": "stripe"},
		})
	}
	return nil
}

func (h *StripeHandler) CancelSubscription(c *gin.Context) {
	var req struct {
		LicenseID string `json:"license_id" binding:"required"`
		Immediate bool   `json:"immediate"` // false = cancel at period end (default)
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_id is required")
		return
	}

	lic, err := h.Store.FindLicenseByID(c, req.LicenseID)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}

	emailVal, _ := c.Get("email")
	if e, ok := emailVal.(string); !ok || lic.Email != e {
		response.Forbidden(c, "not your license")
		return
	}

	if lic.StripeSubscriptionID == "" {
		response.BadRequest(c, "no active subscription")
		return
	}

	if req.Immediate {
		_, err = subscription.Cancel(lic.StripeSubscriptionID, nil)
		if err != nil {
			response.Internal(c)
			return
		}
		now := time.Now()
		lic.Status = model.StatusCanceled
		lic.CanceledAt = &now
		lic.ValidUntil = &now
		_ = h.Store.UpdateLicense(c, lic, "status", "canceled_at", "valid_until")
	} else {
		sub, updateErr := subscription.Update(lic.StripeSubscriptionID, &stripe.SubscriptionParams{
			CancelAtPeriodEnd: stripe.Bool(true),
		})
		if updateErr != nil {
			response.Internal(c)
			return
		}
		// Set ValidUntil to when Stripe will cancel the subscription
		periodEnd := time.Unix(sub.CancelAt, 0)
		lic.ValidUntil = &periodEnd
		_ = h.Store.UpdateLicense(c, lic, "valid_until")
	}

	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "cancel_requested",
		ActorType: "user",
		Changes:   map[string]any{"immediate": req.Immediate},
	})

	productName := ""
	if lic.Product != nil {
		productName = lic.Product.Name
	} else if p, err := h.Store.FindProductByID(c, lic.ProductID); err == nil {
		productName = p.Name
	}
	if h.Email != nil {
		h.Email.SendSubscriptionCanceled(lic.Email, productName, req.Immediate)
	}

	response.OK(c, gin.H{
		"status":    "canceled",
		"immediate": req.Immediate,
	})
}

func (h *StripeHandler) ChangePlan(c *gin.Context) {
	var req struct {
		LicenseID  string `json:"license_id" binding:"required"`
		NewPriceID string `json:"new_price_id" binding:"required"`
		Prorate    *bool  `json:"prorate"` // default true
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_id and new_price_id are required")
		return
	}

	// Order matters: license + ownership check FIRST so an attacker
	// probing Bob's session against Alice's license_id can't
	// distinguish "wrong owner" from "bad price" via response codes.
	// Without this, a 400 on an unknown price_id leaks the existence
	// of Alice's license.
	lic, err := h.Store.FindLicenseByID(c, req.LicenseID)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}

	emailVal, _ := c.Get("email")
	if e, ok := emailVal.(string); !ok || lic.Email != e {
		response.Forbidden(c, "not your license")
		return
	}

	newPlan, err := h.Store.FindPlanByStripePrice(c, req.NewPriceID)
	if err != nil || newPlan == nil {
		response.BadRequest(c, "invalid new_price_id")
		return
	}

	if lic.StripeSubscriptionID == "" {
		response.BadRequest(c, "license has no Stripe subscription")
		return
	}

	if newPlan.ProductID != lic.ProductID {
		response.BadRequest(c, "new plan must belong to the same product")
		return
	}

	// Plan availability gates. Without these a leaked price_id for a
	// deprecated / non-subscription plan could be used to side-step
	// the merchant's pricing strategy:
	//   - inactive plans were taken off the public catalogue; honoring
	//     them on change-plan reopens a discontinued tier.
	//   - perpetual / trial plans aren't subscription-billable; Stripe
	//     would happily swap the subscription item but the resulting
	//     license_type wouldn't match the column semantics anywhere
	//     downstream (renewal email, dunning, expiry).
	if !newPlan.Active {
		response.Err(c, http.StatusBadRequest, "PLAN_INACTIVE",
			"target plan is no longer available")
		return
	}
	if newPlan.LicenseType != "subscription" {
		response.Err(c, http.StatusBadRequest, "NOT_SUBSCRIPTION_PLAN",
			"change-plan only accepts subscription plans")
		return
	}

	sub, err := subscription.Get(lic.StripeSubscriptionID, nil)
	if err != nil || len(sub.Items.Data) == 0 {
		response.Internal(c)
		return
	}

	prorationBehavior := "create_prorations"
	if req.Prorate != nil && !*req.Prorate {
		prorationBehavior = "none"
	}

	params := &stripe.SubscriptionParams{
		ProrationBehavior: stripe.String(prorationBehavior),
		Items: []*stripe.SubscriptionItemsParams{
			{
				ID:    stripe.String(sub.Items.Data[0].ID),
				Price: stripe.String(req.NewPriceID),
			},
		},
	}

	updatedSub, err := subscription.Update(lic.StripeSubscriptionID, params)
	if err != nil {
		response.Internal(c)
		return
	}

	oldPlanID := lic.PlanID
	lic.PlanID = newPlan.ID
	_ = h.Store.UpdateLicense(c, lic, "plan_id")

	if subRecord, err := h.Store.FindSubscriptionByLicense(c, lic.ID); err == nil {
		subRecord.PlanID = newPlan.ID
		_ = h.Store.UpdateSubscription(c, subRecord, "plan_id")
	}
	_ = updatedSub // used for audit context

	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "plan_changed",
		ActorType: "user",
		Changes: map[string]any{
			"old_plan_id": oldPlanID, "new_plan_id": newPlan.ID,
			"proration": prorationBehavior,
		},
	})

	response.OK(c, gin.H{
		"status":        "plan_changed",
		"new_plan_id":   newPlan.ID,
		"new_plan_name": newPlan.Name,
		"proration":     prorationBehavior,
	})
}

// licenseForCharge finds the license a charge paid for.
//
//   - One-time purchases: the charge's payment intent is the one the
//     checkout session recorded on the license.
//   - Subscription invoices: older API versions put the invoice id on
//     the charge; current ones (2025-03-31+) don't, so the invoice is
//     found through its payments, filtered by the payment intent.
//     Either way the invoice's subscription names the license.
//   - Customer only: a customer can hold several licenses, so the id
//     alone is trusted only when it names exactly one. Revoking a
//     guess would take a paid license away from the wrong purchase.
//
// nil,nil means no license is attributable (a permanent condition);
// an error means a lookup failed and the event should be retried.
func (h *StripeHandler) licenseForCharge(ctx context.Context, chargeID, paymentIntent, invoiceID, customerID string) (*model.License, error) {
	if paymentIntent != "" {
		lic, err := h.Store.FindLicenseByStripePaymentIntent(ctx, paymentIntent)
		if err == nil {
			return lic, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	if invoiceID != "" {
		inv, err := stripeinvoice.Get(invoiceID, nil)
		if err != nil && !stripeNotFound(err) {
			return nil, err
		}
		if err == nil {
			if lic, err := h.licenseForInvoice(ctx, inv); err != nil || lic != nil {
				return lic, err
			}
		}
	}
	if paymentIntent != "" {
		params := &stripe.InvoicePaymentListParams{
			Payment: &stripe.InvoicePaymentListPaymentParams{
				Type:          stripe.String("payment_intent"),
				PaymentIntent: stripe.String(paymentIntent),
			},
		}
		params.AddExpand("data.invoice")
		params.Filters.AddFilter("limit", "", "1")
		iter := invoicepayment.List(params)
		if iter.Next() {
			if lic, err := h.licenseForInvoice(ctx, iter.InvoicePayment().Invoice); err != nil || lic != nil {
				return lic, err
			}
		} else if err := iter.Err(); err != nil && !stripeNotFound(err) {
			return nil, fmt.Errorf("list invoice payments: %w", err)
		}
	}
	if customerID == "" {
		return nil, nil
	}
	ls, err := h.Store.ListLicensesByStripeCustomer(ctx, customerID)
	if err != nil {
		return nil, err
	}
	if len(ls) == 0 {
		return nil, nil
	}
	if len(ls) > 1 {
		slog.Warn("stripe charge: customer holds several licenses, not acting on charge",
			"charge_id", chargeID, "customer_id", customerID, "licenses", len(ls))
		h.Store.Audit(ctx, &model.AuditLog{
			Entity: "charge", EntityID: chargeID, Action: "unmatched",
			ActorType: "webhook",
			Changes:   map[string]any{"customer_id": customerID, "licenses": len(ls), "provider": "stripe"},
		})
		return nil, nil
	}
	return ls[0], nil
}

func (h *StripeHandler) licenseForInvoice(ctx context.Context, inv *stripe.Invoice) (*model.License, error) {
	if inv == nil || inv.Parent == nil || inv.Parent.SubscriptionDetails == nil || inv.Parent.SubscriptionDetails.Subscription == nil {
		return nil, nil
	}
	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, inv.Parent.SubscriptionDetails.Subscription.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return lic, nil
}

func (h *StripeHandler) onDisputeCreated(ctx context.Context, raw json.RawMessage) {
	var dispute struct {
		ID       string `json:"id"`
		Charge   string `json:"charge"`
		Reason   string `json:"reason"`
		Status   string `json:"status"`
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}
	if json.Unmarshal(raw, &dispute) != nil {
		return
	}

	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "dispute", EntityID: dispute.ID, Action: "created",
		ActorType: "webhook",
		Changes: map[string]any{
			"charge_id": dispute.Charge,
			"reason":    dispute.Reason,
			"amount":    dispute.Amount,
			"status":    dispute.Status,
		},
	})
}

func (h *StripeHandler) onDisputeClosed(ctx context.Context, raw json.RawMessage) {
	var dispute struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal(raw, &dispute) != nil {
		return
	}

	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "dispute", EntityID: dispute.ID, Action: "closed",
		ActorType: "webhook",
		Changes:   map[string]any{"status": dispute.Status, "reason": dispute.Reason},
	})
}

// CreatePortalSession creates a Stripe billing portal session for the user to manage payment methods.
func (h *StripeHandler) CreatePortalSession(c *gin.Context) {
	var req struct {
		LicenseID string `json:"license_id" binding:"required"`
		ReturnURL string `json:"return_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_id is required")
		return
	}

	lic, err := h.Store.FindLicenseByID(c, req.LicenseID)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}

	// Verify ownership
	emailVal, _ := c.Get("email")
	if e, ok := emailVal.(string); !ok || lic.Email != e {
		response.Forbidden(c, "not your license")
		return
	}

	if lic.StripeCustomerID == "" {
		response.BadRequest(c, "no Stripe customer associated")
		return
	}

	returnURL := req.ReturnURL
	if returnURL == "" {
		returnURL = h.BaseURL + "/portal"
	}

	params := &stripe.BillingPortalSessionParams{
		Customer:  stripe.String(lic.StripeCustomerID),
		ReturnURL: stripe.String(returnURL),
	}
	s, err := portalsession.New(params)
	if err != nil {
		response.Internal(c)
		return
	}

	response.OK(c, gin.H{"url": s.URL})
}

// ListInvoices returns invoice history for a license's Stripe customer.
func (h *StripeHandler) ListInvoices(c *gin.Context) {
	licenseID := c.Query("license_id")
	if licenseID == "" {
		response.BadRequest(c, "license_id is required")
		return
	}

	lic, err := h.Store.FindLicenseByID(c, licenseID)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}

	// Verify ownership
	emailVal, _ := c.Get("email")
	if e, ok := emailVal.(string); !ok || lic.Email != e {
		response.Forbidden(c, "not your license")
		return
	}

	if lic.StripeCustomerID == "" {
		response.OK(c, gin.H{"invoices": []any{}})
		return
	}

	params := &stripe.InvoiceListParams{
		Customer: stripe.String(lic.StripeCustomerID),
	}
	params.Filters.AddFilter("limit", "", "20")

	type invoiceItem struct {
		ID          string `json:"id"`
		Number      string `json:"number"`
		Status      string `json:"status"`
		AmountDue   int64  `json:"amount_due"`
		AmountPaid  int64  `json:"amount_paid"`
		Currency    string `json:"currency"`
		Created     int64  `json:"created"`
		PeriodStart int64  `json:"period_start"`
		PeriodEnd   int64  `json:"period_end"`
		InvoicePDF  string `json:"invoice_pdf"`
		HostedURL   string `json:"hosted_url"`
	}

	var invoices []invoiceItem
	iter := stripeinvoice.List(params)
	for iter.Next() {
		inv := iter.Invoice()
		invoices = append(invoices, invoiceItem{
			ID:          inv.ID,
			Number:      inv.Number,
			Status:      string(inv.Status),
			AmountDue:   inv.AmountDue,
			AmountPaid:  inv.AmountPaid,
			Currency:    string(inv.Currency),
			Created:     inv.Created,
			PeriodStart: inv.PeriodStart,
			PeriodEnd:   inv.PeriodEnd,
			InvoicePDF:  inv.InvoicePDF,
			HostedURL:   inv.HostedInvoiceURL,
		})
	}
	if err := iter.Err(); err != nil {
		response.Internal(c)
		return
	}

	response.OK(c, gin.H{"invoices": invoices})
}

func (h *StripeHandler) onPaymentActionRequired(ctx context.Context, raw json.RawMessage) {
	var data invoiceEvent
	if json.Unmarshal(raw, &data) != nil || data.SubscriptionID() == "" {
		return
	}
	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.SubscriptionID())
	if err != nil {
		return
	}
	// Notify user to complete 3DS/SCA authentication
	productName := h.productName(ctx, lic.ProductID)
	if h.Email != nil {
		h.Email.SendPaymentActionRequired(lic.Email, productName, data.HostedInvoiceURL)
	}
	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "payment_action_required",
		ActorType: "webhook", Changes: map[string]any{"provider": "stripe"},
	})
}

func (h *StripeHandler) onSubscriptionPaused(ctx context.Context, raw json.RawMessage) {
	var data struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return
	}
	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.ID)
	if err != nil {
		return
	}
	lic.Status = model.StatusSuspended
	now := time.Now()
	lic.SuspendedAt = &now
	_ = h.Store.UpdateLicenseAndSubscription(ctx, lic, "status", "suspended_at")

	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "suspended",
		ActorType: "webhook", Changes: map[string]any{"reason": "subscription_paused", "provider": "stripe"},
	})
	if h.WebhookSvc != nil {
		h.WebhookSvc.Dispatch(ctx, lic.ProductID, "license.suspended", map[string]any{
			"license_id": lic.ID, "email": lic.Email, "reason": "subscription_paused",
		})
	}
}

func (h *StripeHandler) onSubscriptionResumed(ctx context.Context, raw json.RawMessage) {
	var data struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return
	}
	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.ID)
	if err != nil {
		return
	}
	lic.Status = model.StatusActive
	lic.SuspendedAt = nil
	_ = h.Store.UpdateLicenseAndSubscription(ctx, lic, "status", "suspended_at")

	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "reinstated",
		ActorType: "webhook", Changes: map[string]any{"reason": "subscription_resumed", "provider": "stripe"},
	})
	if h.WebhookSvc != nil {
		h.WebhookSvc.Dispatch(ctx, lic.ProductID, "license.reinstated", map[string]any{
			"license_id": lic.ID, "email": lic.Email,
		})
	}
}

func (h *StripeHandler) onTrialWillEnd(ctx context.Context, raw json.RawMessage) {
	var data struct {
		ID       string `json:"id"`
		TrialEnd int64  `json:"trial_end"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return
	}
	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.ID)
	if err != nil {
		return
	}
	productName := h.productName(ctx, lic.ProductID)
	trialEnd := time.Unix(data.TrialEnd, 0).Format("2006-01-02")
	if h.Email != nil {
		h.Email.SendTrialEnding(lic.Email, productName, trialEnd)
	}
}

func (h *StripeHandler) onInvoiceUpcoming(ctx context.Context, raw json.RawMessage) {
	var data invoiceEvent
	if json.Unmarshal(raw, &data) != nil || data.SubscriptionID() == "" {
		return
	}
	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.SubscriptionID())
	if err != nil {
		return
	}
	// Renewal reminder is handled by expiry checker, but this is a backup from Stripe
	// Just audit it
	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "invoice_upcoming",
		ActorType: "webhook", Changes: map[string]any{"amount_due": data.AmountDue, "currency": data.Currency},
	})
}

func (h *StripeHandler) onCustomerUpdated(ctx context.Context, raw json.RawMessage) {
	var data struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	if json.Unmarshal(raw, &data) != nil || data.ID == "" {
		return
	}
	// Update email on all licenses for this customer
	if data.Email != "" {
		h.Store.UpdateLicenseEmailByStripeCustomer(ctx, data.ID, data.Email)
	}
}

func (h *StripeHandler) productName(ctx context.Context, productID string) string {
	if p, err := h.Store.FindProductByID(ctx, productID); err == nil && p.Name != "" {
		return p.Name
	}
	slog.Warn("product name not found, using fallback", "product_id", productID)
	return "Your Software"
}

// resolvePlanFromLineItems maps a checkout session to a plan through
// the price on its first line item. Keygate creates single-item
// sessions; a multi-item session built elsewhere fulfils its first
// item only.
func (h *StripeHandler) resolvePlanFromLineItems(ctx context.Context, sessionID string) (*model.Plan, error) {
	params := &stripe.CheckoutSessionListLineItemsParams{Session: stripe.String(sessionID)}
	params.Filters.AddFilter("limit", "", "1")
	iter := session.ListLineItems(params)
	if !iter.Next() {
		if err := iter.Err(); err != nil && !stripeNotFound(err) {
			return nil, err
		}
		return nil, nil
	}
	item := iter.LineItem()
	if item.Price == nil || item.Price.ID == "" {
		return nil, nil
	}
	return h.planForPrice(ctx, item.Price.ID)
}

// resolvePlan maps a subscription to a plan through its first item's
// price. nil,nil when the subscription has no items or the price is
// not mapped; an error for Stripe or database failures.
func (h *StripeHandler) resolvePlan(ctx context.Context, subID string) (*model.Plan, error) {
	sub, err := subscription.Get(subID, nil)
	if err != nil {
		if stripeNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(sub.Items.Data) == 0 {
		return nil, nil
	}
	return h.planForPrice(ctx, sub.Items.Data[0].Price.ID)
}

// planForPrice distinguishes "no plan uses this price" from a
// database failure.
func (h *StripeHandler) planForPrice(ctx context.Context, priceID string) (*model.Plan, error) {
	plan, err := h.Store.FindPlanByStripePrice(ctx, priceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return plan, nil
}
