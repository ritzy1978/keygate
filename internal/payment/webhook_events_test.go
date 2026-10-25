package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
)

// A full refund and a plan change from the portal each send their
// webhook event, delivered over HTTP to the subscribed endpoint.
func TestRefundAndPlanChangeSendWebhooks(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()

	// The endpoint the merchant subscribed, recording what arrives.
	var mu sync.Mutex
	got := map[string]map[string]any{}
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Event string         `json:"event"`
			Data  map[string]any `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got[body.Event] = body.Data
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()
	waitFor := func(event string) map[string]any {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			mu.Lock()
			data, ok := got[event]
			mu.Unlock()
			if ok {
				return data
			}
		}
		t.Fatalf("no %s webhook arrived", event)
		return nil
	}

	suffix := time.Now().Format("150405.000")
	prod := &model.Product{Name: "Webhook Events", Slug: "whe-" + suffix, Type: "saas"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	if err := s.CreateWebhook(ctx, &model.Webhook{
		ProductID: prod.ID, URL: receiver.URL, Secret: "whsec_test", Active: true,
		Events: []string{"license.revoked", "plan.changed"},
	}); err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	mkPlan := func(name string, licenseType, price string) *model.Plan {
		p := &model.Plan{
			ProductID: prod.ID, Name: name, Slug: "whe-" + strings.ToLower(name) + "-" + suffix,
			LicenseType: licenseType, LicenseModel: "standard", StripePriceID: price, Active: true,
		}
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatalf("create plan %s: %v", name, err)
		}
		return p
	}
	perpetual := mkPlan("Perpetual", "perpetual", "")
	basic := mkPlan("Basic", "subscription", "price_basic_"+suffix)
	pro := mkPlan("Pro", "subscription", "price_pro_"+suffix)

	h := &StripeHandler{Store: s, WebhookSvc: service.NewWebhookService(s, slog.Default(), 2*time.Second, 1, true)}

	// A full refund revokes the purchase and says so.
	bought := &model.License{
		ProductID: prod.ID, PlanID: perpetual.ID, Email: "buyer-" + suffix + "@example.com",
		LicenseKey: "KEY-whe-" + suffix + "-1", Status: model.StatusActive,
		PaymentProvider: "stripe", StripeCustomerID: "cus_whe_" + suffix, StripePaymentIntentID: "pi_whe_" + suffix,
	}
	if err := s.CreateLicense(ctx, bought); err != nil {
		t.Fatalf("create license: %v", err)
	}
	if err := h.onChargeRefunded(ctx, fmt.Appendf(nil, `{"id":"ch_whe_%s","customer":"%s","payment_intent":"%s","refunded":true}`,
		suffix, bought.StripeCustomerID, bought.StripePaymentIntentID)); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if data := waitFor("license.revoked"); data["license_id"] != bought.ID || data["email"] != bought.Email {
		t.Errorf("license.revoked data = %v, want license %s and its email", data, bought.ID)
	}

	// A customer moves their subscription from Basic to Pro in the portal.
	subID := "sub_whe_" + suffix
	sub := &model.License{
		ProductID: prod.ID, PlanID: basic.ID, Email: "subscriber-" + suffix + "@example.com",
		LicenseKey: "KEY-whe-" + suffix + "-2", Status: model.StatusActive,
		PaymentProvider: "stripe", StripeSubscriptionID: subID,
	}
	if err := s.CreateLicense(ctx, sub); err != nil {
		t.Fatalf("create subscription license: %v", err)
	}
	fake := newFakeSubscription(subID, basic.StripePriceID, 1, map[string]int64{basic.StripePriceID: 1000, pro.StripePriceID: 2000})
	stubStripe(t, fake.serve(t))
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/portal/subscription/change-plan",
		bytes.NewBufferString(fmt.Sprintf(`{"license_id":%q,"new_price_id":%q}`, sub.ID, pro.StripePriceID)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("email", sub.Email)
	h.ChangePlan(c)
	if w.Code != http.StatusOK {
		t.Fatalf("change plan = %d: %s", w.Code, w.Body.String())
	}
	data := waitFor("plan.changed")
	if data["license_id"] != sub.ID || data["old_plan_id"] != basic.ID || data["new_plan_id"] != pro.ID {
		t.Errorf("plan.changed data = %v, want %s from %s to %s", data, sub.ID, basic.ID, pro.ID)
	}
}
