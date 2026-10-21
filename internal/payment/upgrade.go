package payment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/checkout/session"
	stripeprice "github.com/stripe/stripe-go/v82/price"
	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

// Upgrading a licence Stripe doesn't bill — a free plan's, issued by an
// admin or claimed through self sign-up — to a paid plan of the same
// product. The customer pays through an ordinary checkout, and the
// session moves *that* licence to the plan: the key and activations
// stay, the entitlements change at the next verify. Without this the
// purchase made a second licence with a new key to activate everywhere.
//
// The session names the licence and the plan in its metadata (kind
// "upgrade"); fulfilCheckout hands such sessions here, from the webhook,
// the success page and the sync alike.

const (
	kindUpgrade = "upgrade"
	metaPlanID  = "plan_id"
)

// upgradeIneligibleProvider records, once per session, that a paid
// upgrade could not be applied, so the audit log says it once.
const upgradeIneligibleProvider = "stripe_upgrade_ineligible"

// errUpgradeIneligible means the licence or plan changed between the
// checkout and its payment so that the upgrade no longer applies.
var errUpgradeIneligible = errors.New("upgrade no longer applies")

// upgradeable reports whether lic can be upgraded by a checkout: Stripe
// does not bill it already (that licence changes plan through
// change-plan) and nobody suspended or revoked it. A lapsed one may
// still upgrade — buying is how it comes back.
func upgradeable(lic *model.License) bool {
	if lic.StripeSubscriptionID != "" || lic.PaymentProvider == "stripe" {
		return false
	}
	switch lic.Status {
	case model.StatusActive, model.StatusTrialing, model.StatusExpired, model.StatusCanceled:
		return true
	}
	return false
}

// upgradeTarget reports whether lic may be upgraded to plan: a priced,
// active plan of the same product. Plans selling an update period and
// trial plans are left out — the period needs the gating checks a new
// sale gets, and nobody pays to start a trial.
func upgradeTarget(lic *model.License, plan *model.Plan) bool {
	return plan.Active && plan.ProductID == lic.ProductID && plan.ID != lic.PlanID &&
		plan.StripePriceID != "" && plan.LicenseType != "trial" &&
		plan.InitialUpdatesUntil(time.Now()) == nil
}

// StartUpgrade starts a checkout that moves one of the caller's
// licences to a paid plan.
//
// POST /portal/licenses/upgrade  { license_id, plan_id }  → { url }
func (h *StripeHandler) StartUpgrade(c *gin.Context) {
	var req struct {
		LicenseID string `json:"license_id" binding:"required"`
		PlanID    string `json:"plan_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_id and plan_id are required")
		return
	}
	lic, err := h.Store.FindLicenseByID(c, req.LicenseID)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}
	// Only the owner buys for a licence, as with renewals and plan
	// changes; seat holders use it but do not pay for it.
	emailVal, _ := c.Get("email")
	if e, ok := emailVal.(string); !ok || !strings.EqualFold(lic.Email, e) {
		response.Forbidden(c, "not your license")
		return
	}
	plan, err := h.Store.FindPlanByID(c, req.PlanID)
	if err != nil {
		response.NotFound(c, "plan not found")
		return
	}
	if !upgradeable(lic) || !upgradeTarget(lic, plan) {
		response.Err(c, http.StatusBadRequest, "UPGRADE_NOT_AVAILABLE", "this license cannot be upgraded to that plan")
		return
	}

	// Stripe decides what the price is, as for CheckoutByPlan; the
	// session mode has to match the licence the plan issues, or the
	// customer would pay once for a subscription or monthly for a
	// perpetual licence.
	sp, perr := stripeprice.Get(plan.StripePriceID, nil)
	mode := stripe.CheckoutSessionModeSubscription
	if perr == nil && sp.Type == "one_time" {
		mode = stripe.CheckoutSessionModePayment
	}
	wantMode := stripe.CheckoutSessionModeSubscription
	if plan.LicenseType == "perpetual" {
		wantMode = stripe.CheckoutSessionModePayment
	}
	if perr != nil || !sp.Active || mode != wantMode {
		slog.Error("stripe upgrade: the plan's price cannot be sold as this plan's licence type",
			"plan_id", plan.ID, "price_id", plan.StripePriceID, "license_type", plan.LicenseType, "error", perr)
		response.Err(c, http.StatusServiceUnavailable, "UPGRADE_UNAVAILABLE",
			"upgrades to this plan are temporarily unavailable; please try again later")
		return
	}

	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(mode)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{Price: stripe.String(plan.StripePriceID), Quantity: stripe.Int64(1)},
		},
		SuccessURL:          stripe.String(h.BaseURL + "/checkout/success?session_id={CHECKOUT_SESSION_ID}"),
		CancelURL:           stripe.String(h.BaseURL + "/portal"),
		AllowPromotionCodes: stripe.Bool(true),
	}
	if lic.StripeCustomerID != "" {
		params.Customer = stripe.String(lic.StripeCustomerID)
	} else {
		params.CustomerEmail = stripe.String(lic.Email)
	}
	params.Metadata = map[string]string{
		metaKind:        kindUpgrade,
		metaLicenseID:   lic.ID,
		metaPlanID:      plan.ID,
		"product_id":    plan.ProductID,
		metaLicenseType: plan.LicenseType,
	}
	if mode == stripe.CheckoutSessionModePayment {
		params.PaymentIntentData = &stripe.CheckoutSessionPaymentIntentDataParams{
			Metadata: map[string]string{metaKind: kindUpgrade, metaLicenseID: lic.ID},
		}
	}
	sess, err := session.New(params)
	if err != nil {
		slog.Error("stripe upgrade: failed to create checkout session", "license_id", lic.ID, "error", err)
		response.Internal(c, err)
		return
	}
	response.OK(c, gin.H{"url": sess.URL})
}

// fulfillUpgrade applies a paid upgrade session to the licence it
// names. Idempotent per session: the licence carries the session id
// afterwards, which is what sessionFulfilled and the unique index on
// it check. Returns false with no error when the upgrade cannot apply
// (the licence went away, was billed or revoked meanwhile); the
// session then stays pending for an operator to refund.
func (h *StripeHandler) fulfillUpgrade(ctx context.Context, customerID, subscriptionID, paymentIntentID string,
	metadata map[string]string, sessionID, source string) (bool, error) {
	licenseID, planID := metadata[metaLicenseID], metadata[metaPlanID]
	if sessionID == "" || licenseID == "" || planID == "" {
		slog.Error("stripe upgrade: session is missing its licence or plan", "session_id", sessionID)
		return false, nil
	}

	claimed, err := h.claimSession(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("claim session: %w", err)
	}
	if !claimed {
		return h.sessionFulfilled(ctx, sessionID)
	}

	var oldPlanID string
	var lic *model.License
	var newPlan *model.Plan
	err = h.Store.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		locked, err := store.LockLicenseIn(ctx, tx, licenseID)
		if errors.Is(err, sql.ErrNoRows) {
			return errUpgradeIneligible
		}
		if err != nil {
			return err
		}
		if err := store.LockReferencedRowsIn(ctx, tx, planID, locked.ProductID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errUpgradeIneligible
			}
			return err
		}
		plan, err := store.FindPlanByIDIn(ctx, tx, planID)
		if err != nil {
			return err
		}
		// The licence and the plan as they read now: both may have
		// changed since the customer opened the checkout. The plan has
		// to issue the licence type that was sold.
		if !upgradeable(locked) || plan.ProductID != locked.ProductID || plan.ID == locked.PlanID ||
			plan.LicenseType != metadata[metaLicenseType] || plan.InitialUpdatesUntil(time.Now()) != nil {
			return errUpgradeIneligible
		}

		oldPlanID = locked.PlanID
		l := *locked
		l.PlanID = plan.ID
		l.Status = model.StatusActive
		// A subscription's end comes from Stripe (invoice.paid and
		// subscription updates), a perpetual licence has none; a free
		// plan's deadline goes either way.
		l.ValidUntil = nil
		l.CanceledAt = nil
		l.PastDueAt = nil
		// The target sells no update period: a subscription follows
		// valid_until, a perpetual licence gets updates for life.
		l.UpdatesUntil = nil
		l.UpdatesTermsSet = true
		l.PaymentProvider = "stripe"
		l.StripeCustomerID = customerID
		l.StripeSubscriptionID = subscriptionID
		l.StripePaymentIntentID = paymentIntentID
		l.StripeCheckoutSessionID = sessionID
		update := store.UpdateLicenseIn
		if locked.UpdatesUntil != nil {
			// The old period's renewals belong to a period this licence
			// no longer has.
			update = store.UpdateLicenseAndSupersedeRenewalsIn
		}
		if err := update(ctx, tx, &l,
			"plan_id", "status", "valid_until", "canceled_at", "past_due_at",
			"updates_until", "updates_terms_set", "payment_provider", "stripe_customer_id",
			"stripe_subscription_id", "stripe_payment_intent_id", "stripe_checkout_session_id"); err != nil {
			return err
		}
		if err := store.SyncLicenseSubscriptionIn(ctx, tx, l.ID, plan, l.Status, l.ValidUntil); err != nil {
			return err
		}
		lic, newPlan = &l, plan
		return nil
	})
	switch {
	case err == nil:
	case store.IsCheckoutSessionConflict(err):
		slog.Warn("stripe upgrade: session applied concurrently", "session_id", sessionID)
		return true, nil
	case errors.Is(err, errUpgradeIneligible):
		if derr := h.release(ctx, fulfilledSessionProvider, sessionClaimProvider, sessionID); derr != nil {
			slog.Error("stripe upgrade: failed to release session claim", "session_id", sessionID, "error", derr)
		}
		if first, _ := h.Store.ClaimProcessedEvent(ctx, upgradeIneligibleProvider, sessionID); first {
			h.Store.Audit(ctx, &model.AuditLog{
				Entity: "license", EntityID: licenseID, Action: "upgrade_ineligible",
				ActorType: "webhook",
				Changes: map[string]any{
					"session_id": sessionID, "plan_id": planID, "payment_intent": paymentIntentID,
					"subscription": subscriptionID, "provider": "stripe", "source": source,
				},
			})
		}
		slog.Warn("stripe upgrade: paid upgrade cannot be applied — the licence was removed, revoked or is billed by Stripe already, or the plan changed; refund it",
			"license_id", licenseID, "plan_id", planID, "session_id", sessionID, "source", source)
		return false, nil
	default:
		if derr := h.release(ctx, fulfilledSessionProvider, sessionClaimProvider, sessionID); derr != nil {
			slog.Error("stripe upgrade: failed to release session claim", "session_id", sessionID, "error", derr)
		}
		return false, fmt.Errorf("apply upgrade: %w", err)
	}
	if err := h.Store.CompleteProcessedEvent(ctx, sessionClaimProvider, sessionID); err != nil {
		slog.Error("stripe upgrade: failed to record session completion", "session_id", sessionID, "error", err)
	}
	_ = h.Store.DeleteProcessedEvent(ctx, pendingSessionProvider, sessionID)
	_ = h.Store.DeleteProcessedEvent(ctx, upgradeIneligibleProvider, sessionID)

	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "plan_changed",
		ActorType: "webhook",
		Changes: map[string]any{
			"old_plan_id": oldPlanID, "new_plan_id": newPlan.ID, "kind": kindUpgrade,
			"session_id": sessionID, "provider": "stripe", "source": source,
		},
	})
	if h.WebhookSvc != nil {
		h.WebhookSvc.Dispatch(ctx, lic.ProductID, model.EventPlanChanged, map[string]any{
			"license_id": lic.ID, "old_plan_id": oldPlanID, "new_plan_id": newPlan.ID,
		})
	}
	if h.Email != nil && lic.Email != "" {
		productName, oldPlanName := "", ""
		if p, err := h.Store.FindProductByID(ctx, lic.ProductID); err == nil {
			productName = p.Name
		}
		if p, err := h.Store.FindPlanByID(ctx, oldPlanID); err == nil {
			oldPlanName = p.Name
		}
		h.Email.SendPlanChanged(lic.Email, productName, oldPlanName, newPlan.Name)
	}
	slog.Info("stripe upgrade: applied", "license_id", lic.ID, "old_plan", oldPlanID, "new_plan", newPlan.ID, "source", source)
	return true, nil
}
