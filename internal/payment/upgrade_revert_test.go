package payment

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// metaFor is meta for an upgrade to plan.
func (f *upgradeFixture) metaFor(plan *model.Plan, sessionID string) map[string]string {
	m := f.meta(sessionID)
	m[metaPlanID], m[metaLicenseType] = plan.ID, plan.LicenseType
	return m
}

// lifetimePlan adds a priced perpetual plan, bought with a one-time payment.
func (f *upgradeFixture) lifetimePlan(t *testing.T) *model.Plan {
	t.Helper()
	p := &model.Plan{ProductID: f.lic.ProductID, Name: "Lifetime", Slug: "life-" + f.tag, LicenseType: "perpetual",
		LicenseModel: "standard", MaxActivations: 1, StripePriceID: "price_life_" + f.tag}
	if err := f.s.CreatePlan(f.ctx, p); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return p
}

func (f *upgradeFixture) reload(t *testing.T) *model.License {
	t.Helper()
	l, err := f.s.FindLicenseByID(f.ctx, f.lic.ID)
	if err != nil {
		t.Fatalf("reload licence: %v", err)
	}
	return l
}

func (f *upgradeFixture) upgradeRow(t *testing.T, sessionID string) *model.LicenseUpgrade {
	t.Helper()
	u, err := f.s.FindLicenseUpgradeBySession(f.ctx, sessionID)
	if err != nil {
		t.Fatalf("upgrade row for %s: %v", sessionID, err)
	}
	return u
}

// applySub pays a subscription upgrade to the paid plan.
func (f *upgradeFixture) applySub(t *testing.T, sessionID, subID string) {
	t.Helper()
	ok, err := f.h.fulfillCheckout(f.ctx, f.lic.Email, "cus_"+f.tag, subID, "", f.meta(sessionID), "webhook")
	if err != nil || !ok {
		t.Fatalf("apply %s: ok=%v err=%v", sessionID, ok, err)
	}
}

// assertBackOnFree checks the licence is the free one it was: same key,
// on the free plan, active, and no longer billed by Stripe.
func (f *upgradeFixture) assertBackOnFree(t *testing.T, sessionID, reason string) {
	t.Helper()
	l := f.reload(t)
	switch {
	case l.KeyHash != f.lic.KeyHash:
		t.Fatalf("key changed")
	case l.PlanID != f.free.ID:
		t.Fatalf("plan %s, want the free plan back", l.PlanID)
	case l.Status != model.StatusActive:
		t.Fatalf("status %q, want active", l.Status)
	case l.PaymentProvider != "" || l.StripeSubscriptionID != "" || l.StripePaymentIntentID != "" || l.StripeCheckoutSessionID != "" ||
		l.StripeCustomerID != "":
		t.Fatalf("still linked to Stripe: provider=%q sub=%q pi=%q session=%q",
			l.PaymentProvider, l.StripeSubscriptionID, l.StripePaymentIntentID, l.StripeCheckoutSessionID)
	case l.CanceledAt != nil:
		t.Fatalf("canceled_at %v, want none", l.CanceledAt)
	}
	u := f.upgradeRow(t, sessionID)
	if u.State != model.UpgradeReverted || u.EndReason != reason {
		t.Fatalf("upgrade row state=%q reason=%q, want reverted/%s", u.State, u.EndReason, reason)
	}
	if sub, err := f.s.FindSubscriptionByLicense(f.ctx, f.lic.ID); err == nil && sub.PlanID != f.free.ID {
		t.Fatalf("subscription row still on plan %s", sub.PlanID)
	}
}

// A full refund of a one-time upgrade puts the licence back on its free
// plan with the same key, instead of revoking a licence the customer
// had before paying. Replays of the session and of the refund change
// nothing more.
func TestUpgrade_RefundPutsTheLicenceBack(t *testing.T) {
	f := newUpgradeFixture(t, "uprefund")
	life := f.lifetimePlan(t)
	sessionID, pi := "cs_rf_"+f.tag, "pi_rf_"+f.tag
	if ok, err := f.h.fulfillCheckout(f.ctx, f.lic.Email, "cus_"+f.tag, "", pi, f.metaFor(life, sessionID), "webhook"); err != nil || !ok {
		t.Fatalf("apply: ok=%v err=%v", ok, err)
	}
	u := f.upgradeRow(t, sessionID)
	if u.State != model.UpgradeApplied || u.FromPlanID != f.free.ID || u.FromStatus != model.StatusActive || u.StripePaymentIntentID != pi {
		t.Fatalf("applied row: %+v", u)
	}
	if l := f.reload(t); l.PlanID != life.ID || l.PaymentProvider != "stripe" {
		t.Fatalf("not upgraded: plan=%s provider=%q", l.PlanID, l.PaymentProvider)
	}

	refund := fmt.Appendf(nil, `{"id":"ch_rf_%s","payment_intent":"%s","amount":4900,"amount_refunded":4900,"refunded":true}`, f.tag, pi)
	if err := f.h.onChargeRefunded(f.ctx, refund); err != nil {
		t.Fatalf("refund: %v", err)
	}
	f.assertBackOnFree(t, sessionID, upgradeEndRefund)

	for _, source := range []string{"webhook", "verify", "sync"} {
		if ok, err := f.h.fulfillCheckout(f.ctx, f.lic.Email, "cus_"+f.tag, "", pi, f.metaFor(life, sessionID), source); err != nil || !ok {
			t.Fatalf("%s replay: ok=%v err=%v", source, ok, err)
		}
	}
	if err := f.h.onChargeRefunded(f.ctx, refund); err != nil {
		t.Fatalf("second refund: %v", err)
	}
	f.assertBackOnFree(t, sessionID, upgradeEndRefund)
}

// A refund that arrives before the upgrade was applied closes it: a
// later delivery of the session does not apply it.
func TestUpgrade_RefundBeforeItApplies(t *testing.T) {
	f := newUpgradeFixture(t, "upearly")
	life := f.lifetimePlan(t)
	sessionID, pi := "cs_er_"+f.tag, "pi_er_"+f.tag
	u := &model.LicenseUpgrade{LicenseID: f.lic.ID, ToPlanID: life.ID, StripeCheckoutSessionID: sessionID}
	if err := store.CreateLicenseUpgradeIn(f.ctx, f.s.DB, u); err != nil {
		t.Fatal(err)
	}
	refund := fmt.Appendf(nil, `{"id":"ch_er_%s","payment_intent":"%s","refunded":true,"metadata":{"kind":"upgrade","upgrade_id":"%s","license_id":"%s"}}`,
		f.tag, pi, u.ID, f.lic.ID)
	if err := f.h.onChargeRefunded(f.ctx, refund); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if got := f.upgradeRow(t, sessionID); got.State != model.UpgradeRefunded {
		t.Fatalf("row state %q, want refunded", got.State)
	}
	meta := f.metaFor(life, sessionID)
	meta[metaUpgradeID] = u.ID
	if ok, err := f.h.fulfillCheckout(f.ctx, f.lic.Email, "cus_"+f.tag, "", pi, meta, "sync"); err != nil || !ok {
		t.Fatalf("delivery after the refund: ok=%v err=%v", ok, err)
	}
	if l := f.reload(t); l.PlanID != f.free.ID || l.PaymentProvider != "" || l.Status != model.StatusActive {
		t.Fatalf("refunded upgrade applied: plan=%s provider=%q status=%q", l.PlanID, l.PaymentProvider, l.Status)
	}
}

// Whichever way the subscription ends, the licence goes back to its
// free plan, and can be upgraded again afterwards.
func TestUpgrade_SubscriptionEndPutsTheLicenceBack(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		end          func(t *testing.T, f *upgradeFixture, subID string)
	}{
		{"deleted", upgradeEndCanceled, func(t *testing.T, f *upgradeFixture, subID string) {
			f.h.onSubscriptionDeleted(f.ctx, fmt.Appendf(nil, `{"id":"%s"}`, subID))
		}},
		{"canceled", upgradeEndCanceled, func(t *testing.T, f *upgradeFixture, subID string) {
			_ = f.h.onSubscriptionUpdated(f.ctx, fmt.Appendf(nil, `{"id":"%s","status":"canceled"}`, subID))
		}},
		{"unpaid", upgradeEndUnpaid, func(t *testing.T, f *upgradeFixture, subID string) {
			_ = f.h.onSubscriptionUpdated(f.ctx, fmt.Appendf(nil, `{"id":"%s","status":"unpaid"}`, subID))
		}},
		{"past due too long", upgradeEndPastDue, func(t *testing.T, f *upgradeFixture, subID string) {
			l := f.pastDueSince(t, 40*24*time.Hour)
			handled, err := f.h.EndPastDueUpgrade(f.ctx, l, time.Now().Add(-30*24*time.Hour))
			if err != nil || !handled {
				t.Fatalf("the dunning hook did not take the upgraded licence: %v %v", handled, err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUpgradeFixture(t, "upend")
			sessionID, subID := "cs_end_"+f.tag, "sub_end_"+f.tag
			f.applySub(t, sessionID, subID)
			tc.end(t, f, subID)
			f.assertBackOnFree(t, sessionID, tc.reason)

			again, sub2 := "cs_end2_"+f.tag, "sub_end2_"+f.tag
			f.applySub(t, again, sub2)
			if l := f.reload(t); l.PlanID != f.paid.ID || l.StripeSubscriptionID != sub2 {
				t.Fatalf("second upgrade not applied: plan=%s sub=%q", l.PlanID, l.StripeSubscriptionID)
			}
		})
	}
}

// The dunning sweep leaves a licence no upgrade moved to its usual
// handling.
func TestEndPastDueUpgrade_OrdinaryLicence(t *testing.T) {
	f := newUpgradeFixture(t, "updunning")
	if handled, err := f.h.EndPastDueUpgrade(f.ctx, f.reload(t), time.Now()); err != nil || handled {
		t.Fatalf("the hook took a licence no upgrade moved: %v %v", handled, err)
	}
}

// What an operator did since the upgrade stands: a licence they
// suspended keeps its suspension, and one they unlinked from the
// payment is not touched at all — in particular not revoked by the
// refund, as a purchase would be.
func TestUpgrade_RevertKeepsOperatorChanges(t *testing.T) {
	t.Run("suspended", func(t *testing.T) {
		f := newUpgradeFixture(t, "upsusp")
		sessionID, subID := "cs_su_"+f.tag, "sub_su_"+f.tag
		f.applySub(t, sessionID, subID)
		l := f.reload(t)
		l.Status, l.SuspendedBy = model.StatusSuspended, model.SuspendedByAdmin
		if err := f.s.UpdateLicense(f.ctx, l, "status", "suspended_by"); err != nil {
			t.Fatal(err)
		}
		f.h.onSubscriptionDeleted(f.ctx, fmt.Appendf(nil, `{"id":"%s"}`, subID))
		got := f.reload(t)
		if got.PlanID != f.free.ID || got.Status != model.StatusSuspended || got.SuspendedBy != model.SuspendedByAdmin {
			t.Fatalf("plan=%s status=%q by=%q, want the free plan and the suspension kept", got.PlanID, got.Status, got.SuspendedBy)
		}
	})
	t.Run("unlinked", func(t *testing.T) {
		f := newUpgradeFixture(t, "upunlink")
		life := f.lifetimePlan(t)
		sessionID, pi := "cs_ul_"+f.tag, "pi_ul_"+f.tag
		if ok, err := f.h.fulfillCheckout(f.ctx, f.lic.Email, "", "", pi, f.metaFor(life, sessionID), "webhook"); err != nil || !ok {
			t.Fatalf("apply: ok=%v err=%v", ok, err)
		}
		l := f.reload(t)
		l.StripeCheckoutSessionID, l.PaymentProvider = "", "manual"
		if err := f.s.UpdateLicense(f.ctx, l, "stripe_checkout_session_id", "payment_provider"); err != nil {
			t.Fatal(err)
		}
		refund := fmt.Appendf(nil, `{"id":"ch_ul_%s","payment_intent":"%s","refunded":true}`, f.tag, pi)
		if err := f.h.onChargeRefunded(f.ctx, refund); err != nil {
			t.Fatalf("refund: %v", err)
		}
		got := f.reload(t)
		if got.PlanID != life.ID || got.Status != model.StatusActive {
			t.Fatalf("unlinked licence changed: plan=%s status=%q", got.PlanID, got.Status)
		}
		if u := f.upgradeRow(t, sessionID); u.State != model.UpgradeReverted || !strings.HasSuffix(u.EndReason, ":unlinked") {
			t.Fatalf("row state=%q reason=%q", u.State, u.EndReason)
		}
	})
}

// Only a licence on a free plan upgrades. One an admin issued on a
// priced plan (paid by invoice, sold by a reseller) is left alone: an
// "upgrade" of a lifetime licence to a monthly plan would end it with
// the subscription.
func TestUpgrade_OnlyFromAFreePlan(t *testing.T) {
	f := newUpgradeFixture(t, "upfree")
	life := f.lifetimePlan(t)
	l := f.reload(t)
	l.PlanID = life.ID
	if err := f.s.UpdateLicense(f.ctx, l, "plan_id"); err != nil {
		t.Fatal(err)
	}
	if upgradeable(l, life) {
		t.Fatal("a licence on a priced plan reads as upgradeable")
	}
	ok, err := f.h.fulfillCheckout(f.ctx, f.lic.Email, "", "sub_pf_"+f.tag, "", f.meta("cs_pf_"+f.tag), "webhook")
	if err != nil || ok {
		t.Fatalf("upgrade from a priced plan: ok=%v err=%v", ok, err)
	}
	if got := f.reload(t); got.PlanID != life.ID || got.StripeSubscriptionID != "" {
		t.Fatalf("licence changed: plan=%s sub=%q", got.PlanID, got.StripeSubscriptionID)
	}
}

// Two paid sessions for one licence: one applies, the other is left for
// the operator, and the licence carries exactly one.
func TestUpgrade_ConcurrentSessionsApplyOnce(t *testing.T) {
	f := newUpgradeFixture(t, "uprace")
	const n = 4
	oks := make([]bool, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id := fmt.Sprintf("%s_%d", f.tag, i)
			ok, err := f.h.fulfillCheckout(f.ctx, f.lic.Email, "cus_"+f.tag, "sub_rc_"+id, "", f.meta("cs_rc_"+id), "webhook")
			if err != nil {
				t.Errorf("session %d: %v", i, err)
			}
			oks[i] = ok
		}()
	}
	close(start)
	wg.Wait()
	applied := 0
	for _, ok := range oks {
		if ok {
			applied++
		}
	}
	var rows int
	if err := f.s.DB.NewRaw("SELECT count(*) FROM license_upgrades WHERE license_id = ? AND state = ?", f.lic.ID, model.UpgradeApplied).
		Scan(f.ctx, &rows); err != nil {
		t.Fatal(err)
	}
	if applied != 1 || rows != 1 {
		t.Fatalf("%d sessions applied, %d applied rows, want 1 and 1", applied, rows)
	}
}

// The portal offers Upgrade only where it can work: the owner's licence
// on a free plan, with Stripe configured and a paid plan to move to.
func TestUpgradableLicenses(t *testing.T) {
	f := newUpgradeFixture(t, "upflag")
	ls, err := f.s.ListLicensesByEmail(f.ctx, f.lic.Email)
	if err != nil || len(ls) != 1 {
		t.Fatalf("list: %v %d", err, len(ls))
	}
	if got, _ := f.h.UpgradableLicenses(f.ctx, f.lic.Email, ls); got[f.lic.ID] {
		t.Fatal("offered without Stripe configured")
	}
	prev := stripe.Key
	stripe.Key = "sk_test_flag"
	defer func() { stripe.Key = prev }()
	if got, _ := f.h.UpgradableLicenses(f.ctx, f.lic.Email, ls); !got[f.lic.ID] {
		t.Fatal("not offered to the owner of a free licence")
	}
	if got, _ := f.h.UpgradableLicenses(f.ctx, "seat@example.com", ls); got[f.lic.ID] {
		t.Fatal("offered to someone who is not the owner")
	}
	f.paid.Active = false
	if err := f.s.UpdatePlan(f.ctx, f.paid, "active"); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.h.UpgradableLicenses(f.ctx, f.lic.Email, ls); got[f.lic.ID] {
		t.Fatal("offered with no paid plan to move to")
	}
}

// pastDueSince marks the licence past due since ago, as dunning would.
func (f *upgradeFixture) pastDueSince(t *testing.T, ago time.Duration) *model.License {
	t.Helper()
	l := f.reload(t)
	since := time.Now().Add(-ago)
	l.Status, l.PastDueAt = model.StatusPastDue, &since
	if err := f.s.UpdateLicense(f.ctx, l, "status", "past_due_at"); err != nil {
		t.Fatal(err)
	}
	return l
}

// The sweep reads its candidates first and acts later. A customer who
// paid in between keeps the subscription: the hook reads the licence
// again before cancelling anything.
func TestEndPastDueUpgrade_PaidMeanwhile(t *testing.T) {
	f := newUpgradeFixture(t, "uppaid")
	sessionID, subID := "cs_pd_"+f.tag, "sub_pd_"+f.tag
	f.applySub(t, sessionID, subID)
	stale := f.pastDueSince(t, 40*24*time.Hour)
	recovered := f.reload(t)
	recovered.Status, recovered.PastDueAt = model.StatusActive, nil
	if err := f.s.UpdateLicense(f.ctx, recovered, "status", "past_due_at"); err != nil {
		t.Fatal(err)
	}
	handled, err := f.h.EndPastDueUpgrade(f.ctx, stale, time.Now().Add(-30*24*time.Hour))
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v, want the sweep to leave it alone", handled, err)
	}
	if l := f.reload(t); l.PlanID != f.paid.ID || l.StripeSubscriptionID != subID || l.Status != model.StatusActive {
		t.Fatalf("paid licence changed: plan=%s sub=%q status=%q", l.PlanID, l.StripeSubscriptionID, l.Status)
	}
	if u := f.upgradeRow(t, sessionID); u.State != model.UpgradeApplied {
		t.Fatalf("upgrade %s, want still applied", u.State)
	}
}

// On current API versions a charge has no invoice field: the refund of
// a subscription upgrade is traced through the payment's invoice to the
// subscription, which is cancelled, and the licence goes back to free
// rather than being revoked.
func TestUpgrade_SubscriptionRefundWithoutInvoiceField(t *testing.T) {
	f := newUpgradeFixture(t, "upbasil")
	sessionID, subID, pi := "cs_bs_"+f.tag, "sub_bs_"+f.tag, "pi_bs_"+f.tag
	f.applySub(t, sessionID, subID)
	var cancels atomic.Int32
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/invoice_payments":
			fmt.Fprintf(w, `{"object":"list","has_more":false,"url":"/v1/invoice_payments","data":[{"id":"inpay_%[1]s","object":"invoice_payment",
				"invoice":{"id":"in_%[1]s","object":"invoice","parent":{"type":"subscription_details","subscription_details":{"subscription":"%[2]s","metadata":{}}}}}]}`, f.tag, subID)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/subscriptions/"+subID:
			cancels.Add(1)
			fmt.Fprintf(w, `{"id":"%s","object":"subscription","status":"canceled"}`, subID)
		default:
			http.Error(w, `{"error":{"message":"unexpected `+r.Method+` `+r.URL.Path+`"}}`, http.StatusNotFound)
		}
	})
	refund := fmt.Appendf(nil, `{"id":"ch_bs_%s","payment_intent":"%s","customer":"cus_%s","refunded":true}`, f.tag, pi, f.tag)
	if err := f.h.onChargeRefunded(f.ctx, refund); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if cancels.Load() != 1 {
		t.Fatalf("subscription cancelled %d times, want 1", cancels.Load())
	}
	f.assertBackOnFree(t, sessionID, upgradeEndRefund)
}

// A cancel that fails is retried: the event errors and the licence stays
// as it was, rather than ending canceled on the paid plan with nothing
// left to bring it back.
func TestUpgrade_FailedCancelIsRetried(t *testing.T) {
	f := newUpgradeFixture(t, "upretry")
	sessionID, subID := "cs_rt_"+f.tag, "sub_rt_"+f.tag
	f.applySub(t, sessionID, subID)
	var cancelOK, canceled atomic.Bool
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions/"+subID:
			status := "unpaid"
			if canceled.Load() {
				status = "canceled"
			}
			fmt.Fprintf(w, `{"id":"%s","object":"subscription","status":"%s","items":{"data":[]}}`, subID, status)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/subscriptions/"+subID:
			if !cancelOK.Load() {
				http.Error(w, `{"error":{"message":"try again"}}`, http.StatusInternalServerError)
				return
			}
			canceled.Store(true)
			fmt.Fprintf(w, `{"id":"%s","object":"subscription","status":"canceled"}`, subID)
		default:
			http.Error(w, `{"error":{"message":"unexpected"}}`, http.StatusNotFound)
		}
	})
	event := fmt.Appendf(nil, `{"id":"%s","status":"unpaid"}`, subID)
	if err := f.h.onSubscriptionUpdated(f.ctx, event); err == nil {
		t.Fatal("a failed cancel was acknowledged")
	}
	if l := f.reload(t); l.PlanID != f.paid.ID || l.Status == model.StatusCanceled {
		t.Fatalf("licence moved on a failed cancel: plan=%s status=%q", l.PlanID, l.Status)
	}
	cancelOK.Store(true)
	if err := f.h.onSubscriptionUpdated(f.ctx, event); err != nil {
		t.Fatalf("retry: %v", err)
	}
	f.assertBackOnFree(t, sessionID, upgradeEndUnpaid)
}

// Paid for a subscription that had already ended: the upgrade closes as
// void, and the close sticks (it used to be rolled back with the
// transaction, leaving the upgrade pending for good).
func TestUpgrade_SubscriptionEndedBeforeItApplies(t *testing.T) {
	f := newUpgradeFixture(t, "upvoid")
	sessionID, subID := "cs_vd_"+f.tag, "sub_vd_"+f.tag
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions/"+subID {
			fmt.Fprintf(w, `{"id":"%s","object":"subscription","status":"canceled","items":{"data":[]}}`, subID)
			return
		}
		http.Error(w, `{"error":{"message":"unexpected"}}`, http.StatusNotFound)
	})
	ok, err := f.h.fulfillCheckout(f.ctx, f.lic.Email, "cus_"+f.tag, subID, "", f.meta(sessionID), "webhook")
	if err != nil || !ok {
		t.Fatalf("fulfil: ok=%v err=%v", ok, err)
	}
	if u := f.upgradeRow(t, sessionID); u.State != model.UpgradeVoid {
		t.Fatalf("upgrade %s, want void", u.State)
	}
	if l := f.reload(t); l.PlanID != f.free.ID || l.PaymentProvider != "" {
		t.Fatalf("upgrade applied to an ended subscription: plan=%s provider=%q", l.PlanID, l.PaymentProvider)
	}
}

// A checkout whose delayed payment failed closes its upgrade, so the
// licence can open another.
func TestUpgrade_AsyncPaymentFailureClosesTheUpgrade(t *testing.T) {
	f := newUpgradeFixture(t, "upasync")
	sessionID := "cs_as_" + f.tag
	if err := store.CreateLicenseUpgradeIn(f.ctx, f.s.DB, &model.LicenseUpgrade{
		LicenseID: f.lic.ID, ToPlanID: f.paid.ID, StripeCheckoutSessionID: sessionID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.h.onAsyncPaymentFailed(f.ctx, fmt.Appendf(nil, `{"id":"%s"}`, sessionID)); err != nil {
		t.Fatal(err)
	}
	if u := f.upgradeRow(t, sessionID); u.State != model.UpgradeVoid || u.EndReason != "payment_failed" {
		t.Fatalf("upgrade %s/%s, want void/payment_failed", u.State, u.EndReason)
	}
}

// With the free plan deleted there is nothing to put the licence back
// on: the upgrade closes and the refund is handled as a refunded
// purchase, instead of failing and being retried for days.
func TestUpgrade_RefundAfterTheFreePlanWasDeleted(t *testing.T) {
	f := newUpgradeFixture(t, "upgone")
	life := f.lifetimePlan(t)
	sessionID, pi := "cs_gn_"+f.tag, "pi_gn_"+f.tag
	if ok, err := f.h.fulfillCheckout(f.ctx, f.lic.Email, "", "", pi, f.metaFor(life, sessionID), "webhook"); err != nil || !ok {
		t.Fatalf("apply: ok=%v err=%v", ok, err)
	}
	if _, err := f.s.DB.NewDelete().Model((*model.Subscription)(nil)).Where("plan_id = ?", f.free.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeletePlan(f.ctx, f.free.ID); err != nil {
		t.Fatalf("delete the free plan: %v", err)
	}
	refund := fmt.Appendf(nil, `{"id":"ch_gn_%s","payment_intent":"%s","refunded":true}`, f.tag, pi)
	if err := f.h.onChargeRefunded(f.ctx, refund); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if l := f.reload(t); l.Status != model.StatusRevoked {
		t.Fatalf("status %q, want revoked as a refunded purchase", l.Status)
	}
	if u := f.upgradeRow(t, sessionID); u.State != model.UpgradeReverted || !strings.HasSuffix(u.EndReason, ":free_plan_gone") {
		t.Fatalf("upgrade %s/%s", u.State, u.EndReason)
	}
}

// The subscription row outlives each upgrade's Stripe subscription. Its
// cancel state goes when the upgrade does, so a second upgrade is asked
// about afresh instead of inheriting "ends at period end".
func TestUpgrade_CancelStateDoesNotCarryOver(t *testing.T) {
	f := newUpgradeFixture(t, "upcstate")
	sessionID, subID := "cs_cs_"+f.tag, "sub_cs_"+f.tag
	f.applySub(t, sessionID, subID)
	if _, err := f.s.DB.NewUpdate().Model((*model.Subscription)(nil)).
		Set("cancel_at_period_end = true").Set("cancel_state_synced_at = now()").
		Where("license_id = ?", f.lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.h.onSubscriptionDeleted(f.ctx, fmt.Appendf(nil, `{"id":"%s"}`, subID)); err != nil {
		t.Fatal(err)
	}
	f.applySub(t, "cs_cs2_"+f.tag, "sub_cs2_"+f.tag)
	var ends bool
	var synced *time.Time
	if err := f.s.DB.NewRaw("SELECT cancel_at_period_end, cancel_state_synced_at FROM subscriptions WHERE license_id = ? ORDER BY created_at DESC LIMIT 1",
		f.lic.ID).Scan(f.ctx, &ends, &synced); err != nil {
		t.Fatal(err)
	}
	if ends || synced != nil {
		t.Fatalf("second subscription inherited cancel_at_period_end=%v synced=%v", ends, synced)
	}
}

// The grace sweep runs before the dunning sweep and would expire an
// upgraded licence on its paid plan, out of the dunning sweep's reach.
// Its hook puts the licence back on the free plan instead.
func TestEndGraceExpiredUpgrade(t *testing.T) {
	f := newUpgradeFixture(t, "upgrace")
	sessionID, subID := "cs_gr_"+f.tag, "sub_gr_"+f.tag
	f.applySub(t, sessionID, subID)
	l := f.reload(t)
	lapsed := time.Now().Add(-10 * 24 * time.Hour)
	l.ValidUntil = &lapsed
	if err := f.s.UpdateLicense(f.ctx, l, "valid_until"); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	handled, err := f.h.EndGraceExpiredUpgrade(f.ctx, l, cutoff)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	f.assertBackOnFree(t, sessionID, upgradeEndGrace)
}

// Stripe still has the subscription active: only the local period is
// stale. The grace hook syncs it and keeps the licence, rather than
// expiring or reverting a customer who is paying.
func TestEndGraceExpiredUpgrade_StaleLocalPeriod(t *testing.T) {
	f := newUpgradeFixture(t, "upstale")
	sessionID, subID := "cs_st_"+f.tag, "sub_st_"+f.tag
	f.applySub(t, sessionID, subID)
	l := f.reload(t)
	lapsed := time.Now().Add(-10 * 24 * time.Hour)
	l.ValidUntil = &lapsed
	if err := f.s.UpdateLicense(f.ctx, l, "valid_until"); err != nil {
		t.Fatal(err)
	}
	periodEnd := time.Now().Add(20 * 24 * time.Hour).Unix()
	var cancels atomic.Int32
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions/"+subID:
			fmt.Fprintf(w, `{"id":"%s","object":"subscription","status":"active","current_period_end":%d,"items":{"data":[]}}`, subID, periodEnd)
		case r.Method == http.MethodDelete:
			cancels.Add(1)
			http.Error(w, `{"error":{"message":"must not cancel"}}`, http.StatusBadRequest)
		default:
			http.Error(w, `{"error":{"message":"unexpected"}}`, http.StatusNotFound)
		}
	})
	handled, err := f.h.EndGraceExpiredUpgrade(f.ctx, l, time.Now().Add(-7*24*time.Hour))
	if err != nil || !handled || cancels.Load() != 0 {
		t.Fatalf("handled=%v err=%v cancels=%d", handled, err, cancels.Load())
	}
	got := f.reload(t)
	if got.PlanID != f.paid.ID || got.ValidUntil == nil || got.ValidUntil.Unix() != periodEnd {
		t.Fatalf("plan=%s valid_until=%v, want the paid plan with Stripe's period end", got.PlanID, got.ValidUntil)
	}
}

// The subscription's period and status go in with the upgrade, so a
// failing sync after the commit cannot leave a paid licence with no end.
func TestUpgrade_PeriodWrittenWithTheUpgrade(t *testing.T) {
	f := newUpgradeFixture(t, "upperiod")
	sessionID, subID := "cs_pe_"+f.tag, "sub_pe_"+f.tag
	periodEnd := time.Now().Add(30 * 24 * time.Hour).Unix()
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions/"+subID {
			fmt.Fprintf(w, `{"id":"%s","object":"subscription","status":"active","current_period_end":%d,"items":{"data":[]}}`, subID, periodEnd)
			return
		}
		http.Error(w, `{"error":{"message":"unexpected"}}`, http.StatusNotFound)
	})
	f.applySub(t, sessionID, subID)
	got := f.reload(t)
	if got.PlanID != f.paid.ID || got.ValidUntil == nil || got.ValidUntil.Unix() != periodEnd || got.StripeSyncedAt == nil {
		t.Fatalf("plan=%s valid_until=%v synced=%v", got.PlanID, got.ValidUntil, got.StripeSyncedAt)
	}
}

// A refund of an ordinary one-time purchase is handled from the
// database: no Stripe call, so an outage or a key that cannot read
// invoices does not hold the revocation up.
func TestRefund_OneTimePurchaseNeedsNoStripeCall(t *testing.T) {
	f := newUpgradeFixture(t, "upplain")
	life := f.lifetimePlan(t)
	pi := "pi_plain_" + f.tag
	bought := &model.License{ProductID: f.lic.ProductID, PlanID: life.ID, Email: "buyer-" + f.tag + "@example.com",
		LicenseKey: "KEY-plain-" + f.tag, Status: model.StatusActive, PaymentProvider: "stripe", StripePaymentIntentID: pi}
	if err := f.s.CreateLicense(f.ctx, bought); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":{"message":"down"}}`, http.StatusInternalServerError)
	})
	if err := f.h.onChargeRefunded(f.ctx, fmt.Appendf(nil, `{"id":"ch_plain_%s","payment_intent":"%s","refunded":true}`, f.tag, pi)); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("%d Stripe calls for a one-time refund, want none", calls.Load())
	}
	if l, err := f.s.FindLicenseByID(f.ctx, bought.ID); err != nil || l.Status != model.StatusRevoked {
		t.Fatalf("status %q (%v), want revoked", l.Status, err)
	}
}

// The fallback for a deleted free plan survives a retry: the upgrade is
// already closed then, and the refund must still reach the purchase
// path that revokes.
func TestUpgrade_FreePlanGoneFallbackSurvivesRetry(t *testing.T) {
	f := newUpgradeFixture(t, "upgone2")
	life := f.lifetimePlan(t)
	sessionID, pi := "cs_g2_"+f.tag, "pi_g2_"+f.tag
	if ok, err := f.h.fulfillCheckout(f.ctx, f.lic.Email, "", "", pi, f.metaFor(life, sessionID), "webhook"); err != nil || !ok {
		t.Fatalf("apply: ok=%v err=%v", ok, err)
	}
	if _, err := f.s.DB.NewDelete().Model((*model.Subscription)(nil)).Where("plan_id = ?", f.free.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeletePlan(f.ctx, f.free.ID); err != nil {
		t.Fatal(err)
	}
	// The first attempt closes the upgrade, then fails before revoking.
	if handled, _, err := f.h.refundUpgrade(f.ctx, "ch_g2_"+f.tag, pi, "", nil); err != nil || handled {
		t.Fatalf("first attempt: handled=%v err=%v", handled, err)
	}
	if err := f.h.onChargeRefunded(f.ctx, fmt.Appendf(nil, `{"id":"ch_g2_%s","payment_intent":"%s","refunded":true}`, f.tag, pi)); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if l := f.reload(t); l.Status != model.StatusRevoked {
		t.Fatalf("status %q after the retry, want revoked", l.Status)
	}
}

// Paid for a subscription that is unpaid by the time the upgrade is
// applied: it is cancelled, so its open invoice cannot bill later for
// an upgrade that never happened.
func TestUpgrade_UnpaidSubscriptionIsCancelledWhenVoided(t *testing.T) {
	f := newUpgradeFixture(t, "upvoidc")
	sessionID, subID := "cs_vc_"+f.tag, "sub_vc_"+f.tag
	var cancels atomic.Int32
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions/"+subID:
			fmt.Fprintf(w, `{"id":"%s","object":"subscription","status":"unpaid","items":{"data":[]}}`, subID)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/subscriptions/"+subID:
			cancels.Add(1)
			fmt.Fprintf(w, `{"id":"%s","object":"subscription","status":"canceled"}`, subID)
		default:
			http.Error(w, `{"error":{"message":"unexpected"}}`, http.StatusNotFound)
		}
	})
	if ok, err := f.h.fulfillCheckout(f.ctx, f.lic.Email, "", subID, "", f.meta(sessionID), "webhook"); err != nil || !ok {
		t.Fatalf("fulfil: ok=%v err=%v", ok, err)
	}
	if u := f.upgradeRow(t, sessionID); u.State != model.UpgradeVoid || cancels.Load() != 1 {
		t.Fatalf("upgrade %s, cancels %d; want void and one cancel", u.State, cancels.Load())
	}
}
