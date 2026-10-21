package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/license"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

type upgradeFixture struct {
	s          *store.Store
	ctx        context.Context
	h          *StripeHandler
	free, paid *model.Plan
	lic        *model.License
	tag        string
}

// newUpgradeFixture seeds a product with a free and a paid
// subscription plan, and a licence on the free one.
func newUpgradeFixture(t *testing.T, tag string) *upgradeFixture {
	t.Helper()
	s, ctx := openStore(t)
	t.Cleanup(func() { s.Close() })
	suffix := tag + "-" + time.Now().Format("150405.000000")
	prod := &model.Product{Name: tag, Slug: "p-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	free := &model.Plan{ProductID: prod.ID, Name: "Free", Slug: "free-" + suffix, LicenseType: "subscription",
		BillingInterval: "month", LicenseModel: "standard", MaxActivations: 1}
	paid := &model.Plan{ProductID: prod.ID, Name: "Pro", Slug: "pro-" + suffix, LicenseType: "subscription",
		BillingInterval: "month", LicenseModel: "standard", MaxActivations: 1, StripePriceID: "price_up_" + suffix}
	for _, p := range []*model.Plan{free, paid} {
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatalf("create plan: %v", err)
		}
	}
	lic := &model.License{ProductID: prod.ID, PlanID: free.ID, Email: "up-" + suffix + "@example.com",
		LicenseKey: license.GenerateKey(""), Status: model.StatusActive, UpdatesTermsSet: true}
	if err := s.CreateLicenseWithSubscription(ctx, lic, free); err != nil {
		t.Fatalf("create licence: %v", err)
	}
	gin.SetMode(gin.TestMode)
	return &upgradeFixture{s: s, ctx: ctx, h: &StripeHandler{Store: s, BaseURL: "https://keygate.example"},
		free: free, paid: paid, lic: lic, tag: suffix}
}

func (f *upgradeFixture) meta(sessionID string) map[string]string {
	return map[string]string{
		metaKind: kindUpgrade, metaLicenseID: f.lic.ID, metaPlanID: f.paid.ID,
		metaLicenseType: f.paid.LicenseType, "session_id": sessionID,
	}
}

func (f *upgradeFixture) licences(t *testing.T) []model.License {
	t.Helper()
	var ls []model.License
	if err := f.s.DB.NewSelect().Model(&ls).Where("product_id = ?", f.lic.ProductID).Scan(f.ctx); err != nil {
		t.Fatalf("list licences: %v", err)
	}
	return ls
}

// A paid upgrade moves the same licence to the paid plan: same id, same
// key, now billed by the subscription. Replays of the session (webhook
// retry, success page, sync) change nothing more.
func TestUpgrade_KeepsTheLicenceAndKey(t *testing.T) {
	f := newUpgradeFixture(t, "upkeep")
	sessionID, subID := "cs_up_"+f.tag, "sub_up_"+f.tag
	for _, source := range []string{"webhook", "verify", "sync"} {
		ok, err := f.h.fulfillCheckout(f.ctx, f.lic.Email, "cus_up_"+f.tag, subID, "", f.meta(sessionID), source)
		if err != nil || !ok {
			t.Fatalf("%s: ok=%v err=%v", source, ok, err)
		}
	}
	ls := f.licences(t)
	if len(ls) != 1 {
		t.Fatalf("%d licences for the product, want 1", len(ls))
	}
	got := ls[0]
	switch {
	case got.ID != f.lic.ID || got.KeyHash != f.lic.KeyHash:
		t.Fatalf("a different licence came out: %s", got.ID)
	case got.PlanID != f.paid.ID:
		t.Fatalf("plan %s, want the paid plan", got.PlanID)
	case got.PaymentProvider != "stripe" || got.StripeSubscriptionID != subID || got.StripeCustomerID != "cus_up_"+f.tag:
		t.Fatalf("not linked to Stripe: provider=%q sub=%q cus=%q", got.PaymentProvider, got.StripeSubscriptionID, got.StripeCustomerID)
	case got.StripeCheckoutSessionID != sessionID || got.Status != model.StatusActive:
		t.Fatalf("session=%q status=%q", got.StripeCheckoutSessionID, got.Status)
	}
	sub, err := f.s.FindSubscriptionByLicense(f.ctx, f.lic.ID)
	if err != nil || sub.PlanID != f.paid.ID {
		t.Fatalf("subscription row not moved: %+v %v", sub, err)
	}
}

// A licence revoked between checkout and payment is not upgraded; the
// paid session stays pending for the operator to refund.
func TestUpgrade_RevokedLicenceIsNotUpgraded(t *testing.T) {
	f := newUpgradeFixture(t, "uprevoked")
	f.lic.Status = model.StatusRevoked
	if err := f.s.UpdateLicense(f.ctx, f.lic, "status"); err != nil {
		t.Fatal(err)
	}
	ok, err := f.h.fulfillCheckout(f.ctx, f.lic.Email, "", "sub_rv_"+f.tag, "", f.meta("cs_rv_"+f.tag), "webhook")
	if err != nil || ok {
		t.Fatalf("revoked licence: ok=%v err=%v", ok, err)
	}
	got := f.licences(t)
	if len(got) != 1 || got[0].PlanID != f.free.ID || got[0].StripeSubscriptionID != "" {
		t.Fatalf("revoked licence changed: %+v", got)
	}
}

func TestStartUpgrade_Refusals(t *testing.T) {
	f := newUpgradeFixture(t, "upstart")
	call := func(email string, body map[string]string) (int, string) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		var buf bytes.Buffer
		_ = json.NewEncoder(&buf).Encode(body)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/portal/licenses/upgrade", &buf)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("email", email)
		f.h.StartUpgrade(c)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, errCode(out)
	}
	body := map[string]string{"license_id": f.lic.ID, "plan_id": f.paid.ID}
	if code, _ := call("someone-else@example.com", body); code != http.StatusForbidden {
		t.Fatalf("another customer's licence: %d", code)
	}
	// The free plan has no price: it is not something to upgrade to.
	if code, ec := call(f.lic.Email, map[string]string{"license_id": f.lic.ID, "plan_id": f.free.ID}); code != http.StatusBadRequest || ec != "UPGRADE_NOT_AVAILABLE" {
		t.Fatalf("free plan: %d %s", code, ec)
	}
	// A licence Stripe bills already changes plan through change-plan.
	f.lic.PaymentProvider, f.lic.StripeSubscriptionID = "stripe", "sub_billed_"+f.tag
	if err := f.s.UpdateLicense(f.ctx, f.lic, "payment_provider", "stripe_subscription_id"); err != nil {
		t.Fatal(err)
	}
	if code, ec := call(f.lic.Email, body); code != http.StatusBadRequest || ec != "UPGRADE_NOT_AVAILABLE" {
		t.Fatalf("billed licence: %d %s", code, ec)
	}
}
