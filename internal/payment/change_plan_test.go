package payment

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
)

// A plan change in the portal is always prorated, invoiced at once and
// held until that invoice is paid. The customer could once send
// "prorate": false and move to a dearer plan for the rest of the period
// without paying the difference, then move back before the renewal;
// and with the difference left for the next invoice, a dearer plan was
// in use before anything was paid for it.
func TestChangePlan_AlwaysProrates(t *testing.T) {
	s, ctx := openStore(t)
	t.Cleanup(func() { s.Close() })
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Prorate", Slug: "prorate-" + suffix, Type: "saas"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	mkPlan := func(name string) *model.Plan {
		p := &model.Plan{ProductID: prod.ID, Name: name, Slug: "pr-" + name + "-" + suffix, LicenseType: "subscription",
			LicenseModel: "standard", StripePriceID: "price_pr_" + name + "_" + suffix, Active: true}
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatalf("create plan %s: %v", name, err)
		}
		return p
	}
	basic, business := mkPlan("basic"), mkPlan("business")
	subID := "sub_pr_" + suffix
	lic := &model.License{ProductID: prod.ID, PlanID: basic.ID, Email: "prorate-" + suffix + "@example.com",
		LicenseKey: "KEY-pr-" + suffix, Status: model.StatusActive, PaymentProvider: "stripe", StripeSubscriptionID: subID}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatalf("create licence: %v", err)
	}

	fake := newFakeSubscription(subID, basic.StripePriceID, 1, map[string]int64{basic.StripePriceID: 1000, business.StripePriceID: 3000})
	stubStripe(t, fake.serve(t))
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		body string
		want *model.Plan
	}{
		{fmt.Sprintf(`{"license_id":%q,"new_price_id":%q,"prorate":false}`, lic.ID, business.StripePriceID), business},
		{fmt.Sprintf(`{"license_id":%q,"new_price_id":%q}`, lic.ID, basic.StripePriceID), basic},
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/portal/subscription/change-plan", bytes.NewBufferString(tc.body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("email", lic.Email)
		h := &StripeHandler{Store: s}
		h.ChangePlan(c)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"plan_changed"`) {
			t.Fatalf("change plan = %d: %s", w.Code, w.Body.String())
		}
		// The licence follows the subscription Stripe returned.
		got, err := s.FindLicenseByID(ctx, lic.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.PlanID != tc.want.ID {
			t.Fatalf("plan after the change = %s, want %s", got.PlanID, tc.want.ID)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.updates) != 2 {
		t.Fatalf("%d updates sent to Stripe, want 2", len(fake.updates))
	}
	for _, u := range fake.updates {
		if u["proration_behavior"] != "always_invoice" || u["payment_behavior"] != "pending_if_incomplete" {
			t.Fatalf("update sent to Stripe = %v, want always_invoice held until paid", u)
		}
	}
}
