package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
)

// seatFixture is a hybrid product with a plan sold per seat (team), a
// plan sold per licence (solo) and a per-seat plan that starts at ten
// seats (big), and a licence on team billed by one fake Stripe
// subscription. Webhook deliveries are recorded in the database.
type seatFixture struct {
	t      *testing.T
	s      *store.Store
	ctx    context.Context
	suffix string
	prod   *model.Product
	team   *model.Plan
	solo   *model.Plan
	big    *model.Plan
	lic    *model.License
	fake   *fakeSubscription
	h      *StripeHandler
	hookID string
}

func newSeatFixture(t *testing.T, quantity int64) *seatFixture {
	changeRetryWait = 10 * time.Millisecond
	t.Helper()
	s, ctx := openStore(t)
	t.Cleanup(func() { s.Close() })
	f := &seatFixture{t: t, s: s, ctx: ctx, suffix: strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")}
	f.prod = &model.Product{Name: "Seats", Slug: "seats-" + f.suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, f.prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	mk := func(name string, p model.Plan) *model.Plan {
		p.ProductID, p.Name, p.Slug = f.prod.ID, name, "seats-"+name+"-"+f.suffix
		p.LicenseType, p.LicenseModel, p.Active = "subscription", "standard", true
		p.StripePriceID = "price_" + name + "_" + f.suffix
		if err := s.CreatePlan(ctx, &p); err != nil {
			t.Fatalf("create plan %s: %v", name, err)
		}
		return &p
	}
	f.team = mk("team", model.Plan{PerSeat: true, MinQuantity: 1, MaxQuantity: 20, MaxActivations: 2, MaxSeats: 1})
	f.solo = mk("solo", model.Plan{MaxActivations: 3, MaxSeats: 1})
	f.big = mk("big", model.Plan{PerSeat: true, MinQuantity: 10, MaxQuantity: 50, MaxActivations: 1, MaxSeats: 1})
	subID := "sub_seats_" + f.suffix
	f.lic = &model.License{
		ProductID: f.prod.ID, PlanID: f.team.ID, Email: "owner-" + f.suffix + "@example.com",
		LicenseKey: "KEY-SEATS-" + f.suffix, Status: model.StatusActive, PaymentProvider: "stripe",
		StripeSubscriptionID: subID, StripeCustomerID: "cus_" + subID, Quantity: int(quantity),
	}
	if err := s.CreateLicenseWithSubscription(ctx, f.lic, f.team); err != nil {
		t.Fatalf("create license: %v", err)
	}
	f.fake = newFakeSubscription(subID, f.team.StripePriceID, quantity, map[string]int64{
		f.team.StripePriceID: 1000, f.solo.StripePriceID: 500, f.big.StripePriceID: 800,
	})
	stubStripe(t, f.fake.serve(t))

	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(receiver.Close)
	hook := &model.Webhook{ProductID: f.prod.ID, URL: receiver.URL, Secret: "whsec_seats", Active: true,
		Events: []string{model.EventLicenseQuantityChanged, model.EventPlanChanged}}
	if err := s.CreateWebhook(ctx, hook); err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	f.hookID = hook.ID
	f.h = &StripeHandler{Store: s, WebhookSvc: service.NewWebhookService(s, slog.Default(), 2*time.Second, 1, true)}
	gin.SetMode(gin.TestMode)
	return f
}

// post runs a portal handler as the given user.
func (f *seatFixture) post(fn gin.HandlerFunc, email, body string) (int, map[string]any) {
	f.t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/portal/subscription", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("email", email)
	fn(c)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func envData(out map[string]any) map[string]any {
	d, _ := out["data"].(map[string]any)
	return d
}

func envCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

// license reads the licence back.
func (f *seatFixture) license() *model.License {
	f.t.Helper()
	l, err := f.s.FindLicenseByID(f.ctx, f.lic.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return l
}

// deliveries counts the webhook deliveries recorded for event.
func (f *seatFixture) deliveries(event string) int {
	f.t.Helper()
	n, err := f.s.DB.NewSelect().Model((*model.WebhookDelivery)(nil)).
		Where("webhook_id = ? AND event = ?", f.hookID, event).Count(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

// preview asks for a change and returns the preview's data.
func (f *seatFixture) preview(body string) map[string]any {
	f.t.Helper()
	code, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, body)
	if code != http.StatusOK {
		f.t.Fatalf("preview %s = %d %v", body, code, out)
	}
	return envData(out)
}

// change submits a change the way the portal does: with its preview's
// token.
func (f *seatFixture) change(p map[string]any, _ string) (int, map[string]any) {
	f.t.Helper()
	return f.post(f.h.ChangeSubscription, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"preview_token":%q}`, f.lic.ID, p["preview_token"]))
}

// subscriptionUpdated delivers customer.subscription.updated. An event
// whose seats a downgrade holds is asked for again by Stripe later; here
// that is the same as delivered.
func (f *seatFixture) subscriptionUpdated() {
	f.t.Helper()
	if err := f.h.onSubscriptionUpdated(f.ctx, fmt.Appendf(nil, `{"id":%q}`, f.lic.StripeSubscriptionID)); err != nil && !errors.Is(err, store.ErrTermsHeld) {
		f.t.Errorf("subscription.updated: %v", err)
	}
}

// endHold ends a downgrade's hold as if its time were up.
func (f *seatFixture) endHold() {
	f.t.Helper()
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET terms_held_until = now() - interval '1 second' WHERE id = ?", f.lic.ID).Exec(f.ctx); err != nil {
		f.t.Fatal(err)
	}
}

func (f *seatFixture) addMembers(n int) {
	f.t.Helper()
	for i := range n {
		seat := &model.Seat{LicenseID: f.lic.ID, Email: fmt.Sprintf("member%d-%s@example.com", i, f.suffix), Role: "member"}
		if err := f.s.CreateSeat(f.ctx, seat); err != nil {
			f.t.Fatal(err)
		}
	}
}

// The preview shows what Stripe charges for the change, and the credit
// the customer already holds, which Stripe's preview leaves out.
func TestSeats_PreviewShowsWhatStripeCharges(t *testing.T) {
	f := newSeatFixture(t, 2)
	f.fake.balance = -1000
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":5}`, f.lic.ID))
	// Three more seats at 10.00 for half a period.
	if p["total"] != float64(1500) || p["credit_balance"] != float64(1000) || p["due_now"] != float64(500) {
		t.Fatalf("preview amounts = total %v credit %v due %v, want 1500, 1000, 500", p["total"], p["credit_balance"], p["due_now"])
	}
	tgt := p["target"].(map[string]any)
	if tgt["quantity"] != float64(5) || tgt["seat_limit"] != float64(5) || tgt["activation_limit"] != float64(10) {
		t.Fatalf("preview target = %v, want 5 seats, 5 people, 10 devices", tgt)
	}
	f.fake.mu.Lock()
	defer f.fake.mu.Unlock()
	if len(f.fake.updates) != 0 {
		t.Fatalf("a preview changed the subscription: %v", f.fake.updates)
	}
	if pv := f.fake.previews[0]; pv["subscription_details[proration_behavior]"] != "always_invoice" || pv["subscription_details[proration_date]"] == "" {
		t.Fatalf("preview asked Stripe for %v, want always_invoice at a fixed proration date", pv)
	}
}

// More seats, paid: Stripe applies them, and the licence holds them as
// soon as the answer comes back, with one license.quantity_changed.
func TestSeats_IncreasePaidAppliesAtOnce(t *testing.T) {
	f := newSeatFixture(t, 2)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":5}`, f.lic.ID))
	code, out := f.change(p, "")
	if code != http.StatusOK || envData(out)["status"] != "applied" {
		t.Fatalf("change = %d %v, want applied", code, out)
	}
	l := f.license()
	if l.Quantity != 5 {
		t.Fatalf("quantity after a paid change = %d, want 5", l.Quantity)
	}
	if seats, acts := l.Limits(); seats != 5 || acts != 10 {
		t.Fatalf("limits = %d people, %d devices, want 5 and 10", seats, acts)
	}
	f.fake.mu.Lock()
	u := f.fake.updates[0]
	f.fake.mu.Unlock()
	if u["payment_behavior"] != "pending_if_incomplete" || u["proration_behavior"] != "always_invoice" ||
		u["items[0][quantity]"] != "5" || u["proration_date"] != fmt.Sprint(int64(p["proration_date"].(float64))) || u["_idempotency_key"] == "" {
		t.Fatalf("update sent to Stripe = %v", u)
	}
	// The webhook that follows finds nothing new to announce.
	f.subscriptionUpdated()
	if n := f.deliveries(model.EventLicenseQuantityChanged); n != 1 {
		t.Fatalf("%d license.quantity_changed deliveries, want 1", n)
	}
}

// More seats, not paid: Stripe holds the change, and so does the
// licence. The portal is told where to pay, and the seats arrive only
// once the invoice is paid.
func TestSeats_IncreaseUnpaidIsHeld(t *testing.T) {
	f := newSeatFixture(t, 2)
	f.fake.failPayment = true
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":6}`, f.lic.ID))
	code, out := f.change(p, "")
	d := envData(out)
	if code != http.StatusOK || d["status"] != "pending_payment" || d["hosted_invoice_url"] == "" || d["expires_at"] == "" {
		t.Fatalf("change = %d %v, want pending_payment with an invoice to pay", code, out)
	}
	l := f.license()
	if l.Quantity != 2 || l.StripePendingUpdateExpiresAt == nil {
		t.Fatalf("held change: quantity %d, pending %v; want 2 and a pending change", l.Quantity, l.StripePendingUpdateExpiresAt)
	}
	if n := f.deliveries(model.EventLicenseQuantityChanged); n != 0 {
		t.Fatalf("%d license.quantity_changed deliveries for an unpaid change, want 0", n)
	}
	code, out = f.post(f.h.PendingSubscriptionChange, f.lic.Email, fmt.Sprintf(`{"license_id":%q}`, f.lic.ID))
	if code != http.StatusOK || envData(out)["status"] != "pending_payment" || envData(out)["quantity"] != float64(6) {
		t.Fatalf("pending = %d %v, want the six seats waiting", code, out)
	}

	// Paid: Stripe applies it and says so.
	f.fake.pay()
	f.subscriptionUpdated()
	l = f.license()
	if l.Quantity != 6 || l.StripePendingUpdateExpiresAt != nil {
		t.Fatalf("after payment: quantity %d, pending %v; want 6 and none", l.Quantity, l.StripePendingUpdateExpiresAt)
	}
	if n := f.deliveries(model.EventLicenseQuantityChanged); n != 1 {
		t.Fatalf("%d license.quantity_changed deliveries, want 1", n)
	}
}

// Fewer seats than people is refused: nobody is removed by a change.
func TestSeats_DecreaseBelowPeopleRefused(t *testing.T) {
	f := newSeatFixture(t, 4)
	f.addMembers(2) // three people with the owner
	code, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	if code != http.StatusConflict || envCode(out) != "SEATS_IN_USE" {
		t.Fatalf("preview to 2 seats with 3 people = %d %v, want 409 SEATS_IN_USE", code, out)
	}
	// Down to three is fine: a downgrade, nothing charged or credited
	// now, the next invoice on three seats.
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":3}`, f.lic.ID))
	if p["downgrade"] != true || p["total"] != float64(0) || p["due_now"] != float64(0) ||
		p["next_invoice_total"] != float64(3*f.fake.unit[f.team.StripePriceID]) || p["next_invoice_at"] == nil {
		t.Fatalf("preview of fewer seats = %v, want a downgrade billed from the next invoice", p)
	}
	if c, out := f.change(p, ""); c != http.StatusOK || envData(out)["status"] != "applied" {
		t.Fatalf("change to 3 = %d %v", c, out)
	}
	if q := f.license().Quantity; q != 3 {
		t.Fatalf("quantity = %d, want 3", q)
	}
}

// A preview is acted on only as it was shown: not once it is old, and
// not once the subscription moved under it.
func TestSeats_StaleOrExpiredPreviewRefused(t *testing.T) {
	f := newSeatFixture(t, 2)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":5}`, f.lic.ID))

	tok, _ := f.h.openChange(p["preview_token"].(string))
	tok.ProrationDate = time.Now().Add(-2 * time.Hour).Unix()
	if c, out := f.change(map[string]any{"preview_token": f.h.signChange(tok)}, ""); c != http.StatusConflict || envCode(out) != "PREVIEW_EXPIRED" {
		t.Fatalf("change from an old preview = %d %v, want 409 PREVIEW_EXPIRED", c, out)
	}
	// A token altered on the way back (an earlier proration date for a
	// larger credit, more seats for the same price) is no preview.
	body, sig, _ := strings.Cut(p["preview_token"].(string), ".")
	forged := strings.Replace(body, "A", "B", 1) + "." + sig
	if c, out := f.change(map[string]any{"preview_token": forged}, ""); c != http.StatusBadRequest {
		t.Fatalf("change with an altered token = %d %v, want 400", c, out)
	}

	f.fake.set(f.team.StripePriceID, 3) // the seller changed it in Stripe
	if c, out := f.change(p, ""); c != http.StatusConflict || envCode(out) != "SUBSCRIPTION_CHANGED" {
		t.Fatalf("change after the subscription moved = %d %v, want 409 SUBSCRIPTION_CHANGED", c, out)
	}
	f.fake.mu.Lock()
	defer f.fake.mu.Unlock()
	if len(f.fake.updates) != 0 {
		t.Fatalf("a refused change reached Stripe: %v", f.fake.updates)
	}
}

// Only the owner changes what the licence costs, and only while it is
// paid up.
func TestSeats_OwnerAndActiveOnly(t *testing.T) {
	f := newSeatFixture(t, 2)
	body := fmt.Sprintf(`{"license_id":%q,"quantity":5}`, f.lic.ID)
	if c, _ := f.post(f.h.PreviewSubscriptionChange, "someone-else@example.com", body); c != http.StatusForbidden {
		t.Fatalf("preview by another user = %d, want 403", c)
	}
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET status = 'past_due' WHERE id = ?", f.lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	if c, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, body); c != http.StatusConflict || envCode(out) != "SUBSCRIPTION_NOT_ACTIVE" {
		t.Fatalf("preview while past due = %d %v, want 409 SUBSCRIPTION_NOT_ACTIVE", c, out)
	}
}

// A plan whose minimum is above the seats the customer has is never
// reached by quietly raising the seats.
func TestSeats_QuantityNeverMovedIntoRangeSilently(t *testing.T) {
	f := newSeatFixture(t, 3)
	code, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email,
		fmt.Sprintf(`{"license_id":%q,"price_id":%q}`, f.lic.ID, f.big.StripePriceID))
	if code != http.StatusConflict || envCode(out) != "QUANTITY_OUT_OF_RANGE" {
		t.Fatalf("switch to a plan of 10 to 50 seats with 3 = %d %v, want 409 QUANTITY_OUT_OF_RANGE", code, out)
	}
	c, out := f.post(f.h.ChangePlan, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"new_price_id":%q}`, f.lic.ID, f.big.StripePriceID))
	if c != http.StatusConflict || envCode(out) != "QUANTITY_OUT_OF_RANGE" {
		t.Fatalf("change-plan to it = %d %v, want 409 QUANTITY_OUT_OF_RANGE", c, out)
	}
	// Asked for explicitly, it goes through.
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"price_id":%q,"quantity":10}`, f.lic.ID, f.big.StripePriceID))
	if c, out := f.change(p, ""); c != http.StatusOK {
		t.Fatalf("switch with 10 seats = %d %v", c, out)
	}
	l := f.license()
	if l.PlanID != f.big.ID || l.Quantity != 10 {
		t.Fatalf("after the switch: plan %s quantity %d, want big with 10", l.PlanID, l.Quantity)
	}
	if n := f.deliveries(model.EventPlanChanged); n != 1 {
		t.Fatalf("%d plan.changed deliveries, want 1", n)
	}
}

// Moving to a plan not sold per seat leaves one seat.
func TestSeats_ToAPlanNotSoldPerSeat(t *testing.T) {
	f := newSeatFixture(t, 3)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"price_id":%q}`, f.lic.ID, f.solo.StripePriceID))
	if p["target"].(map[string]any)["quantity"] != float64(1) {
		t.Fatalf("target = %v, want one", p["target"])
	}
	if c, out := f.change(p, ""); c != http.StatusOK {
		t.Fatalf("change = %d %v", c, out)
	}
	l := f.license()
	if seats, acts := l.Limits(); l.PlanID != f.solo.ID || l.Quantity != 1 || seats != 1 || acts != 3 {
		t.Fatalf("on solo: plan %s quantity %d limits %d/%d", l.PlanID, l.Quantity, seats, acts)
	}
}

// Stripe saying no changes nothing; Stripe not answering changes
// nothing either, and says so rather than guessing.
func TestSeats_RefusedOrUnansweredChangesNothing(t *testing.T) {
	f := newSeatFixture(t, 2)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":5}`, f.lic.ID))

	f.fake.updateErr = http.StatusBadRequest
	if c, out := f.change(p, ""); c != http.StatusConflict || envCode(out) != "CHANGE_REFUSED" {
		t.Fatalf("change Stripe refused = %d %v, want 409 CHANGE_REFUSED", c, out)
	}
	f.fake.updateErr = http.StatusServiceUnavailable
	if c, out := f.change(p, ""); c != http.StatusAccepted || envData(out)["status"] != "processing" {
		t.Fatalf("change Stripe did not answer = %d %v, want 202 processing", c, out)
	}
	if q := f.license().Quantity; q != 2 {
		t.Fatalf("quantity = %d, want 2 untouched", q)
	}
	// Asking again is the same request to Stripe.
	f.fake.updateErr = 0
	if c, out := f.change(p, ""); c != http.StatusOK {
		t.Fatalf("change again = %d %v", c, out)
	}
	f.fake.mu.Lock()
	defer f.fake.mu.Unlock()
	keys := map[string]bool{}
	for _, u := range f.fake.updates {
		keys[u["_idempotency_key"]] = true
	}
	if len(keys) != 1 {
		t.Fatalf("idempotency keys across the retries = %v, want one", keys)
	}
}

// A subscription the portal cannot change safely is left to the seller.
func TestSeats_UnsupportedSubscriptions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeSubscription)
	}{
		{"metered", func(fs *fakeSubscription) { fs.metered = true }},
		{"invoiced", func(fs *fakeSubscription) { fs.collection = "send_invoice" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSeatFixture(t, 2)
			tc.setup(f.fake)
			c, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"quantity":5}`, f.lic.ID))
			if c != http.StatusConflict || envCode(out) != "CHANGE_NOT_SUPPORTED" {
				t.Fatalf("preview = %d %v, want 409 CHANGE_NOT_SUPPORTED", c, out)
			}
		})
	}
}

// A change made in Stripe itself reaches the licence through the
// subscription's events: its quantity, and the plan of its price. Each
// is announced once, however many events carry it.
func TestSeats_SyncFollowsStripe(t *testing.T) {
	f := newSeatFixture(t, 2)
	f.fake.set(f.team.StripePriceID, 7)
	f.subscriptionUpdated()
	f.subscriptionUpdated()
	if q := f.license().Quantity; q != 7 {
		t.Fatalf("quantity = %d, want 7", q)
	}
	if n := f.deliveries(model.EventLicenseQuantityChanged); n != 1 {
		t.Fatalf("%d license.quantity_changed deliveries, want 1", n)
	}

	f.fake.set(f.solo.StripePriceID, 1)
	f.subscriptionUpdated()
	l := f.license()
	if l.PlanID != f.solo.ID || l.Quantity != 1 {
		t.Fatalf("after a price change in Stripe: plan %s quantity %d, want solo with 1", l.PlanID, l.Quantity)
	}
	var subPlan string
	if err := f.s.DB.NewRaw("SELECT plan_id FROM subscriptions WHERE license_id = ?", f.lic.ID).Scan(f.ctx, &subPlan); err != nil || subPlan != f.solo.ID {
		t.Fatalf("subscription row plan = %q (%v), want %s", subPlan, err, f.solo.ID)
	}
	if n := f.deliveries(model.EventPlanChanged); n != 1 {
		t.Fatalf("%d plan.changed deliveries, want 1", n)
	}
}

// A failed payment for a change Stripe is holding leaves the
// subscription paid and in force: no dunning. A failed renewal still is.
func TestSeats_HeldChangeFailureIsNotDunning(t *testing.T) {
	f := newSeatFixture(t, 2)
	f.fake.failPayment = true
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":6}`, f.lic.ID))
	if c, _ := f.change(p, ""); c != http.StatusOK {
		t.Fatalf("change = %d", c)
	}
	_ = f.h.onPaymentFailed(f.ctx, fmt.Appendf(nil, `{"subscription":%q,"billing_reason":"subscription_update"}`, f.lic.StripeSubscriptionID))
	if l := f.license(); l.Status != model.StatusActive || l.PastDueAt != nil {
		t.Fatalf("after a held change failed to pay: status %s past_due_at %v, want active", l.Status, l.PastDueAt)
	}
	_ = f.h.onPaymentFailed(f.ctx, fmt.Appendf(nil, `{"subscription":%q,"billing_reason":"subscription_cycle"}`, f.lic.StripeSubscriptionID))
	if l := f.license(); l.Status != model.StatusPastDue {
		t.Fatalf("after a renewal failed: status %s, want past_due", l.Status)
	}
}

// Refunding what a change cost gives that back and keeps the licence;
// a full refund of the subscription's own invoice still revokes it.
func TestSeats_ChangeRefundKeepsLicense(t *testing.T) {
	f := newSeatFixture(t, 5)
	f.fake.extra = refundInvoices(f.lic.StripeSubscriptionID)
	if err := f.h.onChargeRefunded(f.ctx, []byte(`{"id":"ch_change","invoice":"in_change","refunded":true,"amount_refunded":1500}`)); err != nil {
		t.Fatal(err)
	}
	if l := f.license(); l.Status != model.StatusActive {
		t.Fatalf("after refunding a change: status %s, want active", l.Status)
	}
	if err := f.h.onChargeRefunded(f.ctx, []byte(`{"id":"ch_cycle","invoice":"in_cycle","refunded":true,"amount_refunded":5000}`)); err != nil {
		t.Fatal(err)
	}
	if l := f.license(); l.Status != model.StatusRevoked {
		t.Fatalf("after refunding the subscription's invoice: status %s, want revoked", l.Status)
	}
}

// The checkout of a plan sold per seat starts at the quantity asked for
// and lets the buyer change it within the plan's range; a plan sold
// per licence sells one.
func TestCheckout_PerSeatQuantity(t *testing.T) {
	f := newSeatFixture(t, 1)
	var sessions []map[string]string
	f.fake.extra = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/prices/"):
			fmt.Fprintf(w, `{"id":%q,"object":"price","type":"recurring","active":true}`, strings.TrimPrefix(r.URL.Path, "/v1/prices/"))
		case r.URL.Path == "/v1/checkout/sessions":
			_ = r.ParseForm()
			form := map[string]string{}
			for k, v := range r.PostForm {
				form[k] = v[0]
			}
			sessions = append(sessions, form)
			_, _ = w.Write([]byte(`{"id":"cs_test","object":"checkout.session","url":"https://checkout.stripe.test/cs_test"}`))
		default:
			return false
		}
		return true
	}
	pay := func(plan *model.Plan, query string) int {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/pay/"+plan.CheckoutID+query, nil)
		c.Params = gin.Params{{Key: "checkout_id", Value: plan.CheckoutID}}
		f.h.CheckoutByPlan(c)
		return w.Code
	}
	if c := pay(f.team, "?quantity=5"); c != http.StatusTemporaryRedirect {
		t.Fatalf("/pay with 5 seats = %d, want a redirect", c)
	}
	s := sessions[len(sessions)-1]
	if s["line_items[0][quantity]"] != "5" || s["line_items[0][adjustable_quantity][enabled]"] != "true" ||
		s["line_items[0][adjustable_quantity][minimum]"] != "1" || s["line_items[0][adjustable_quantity][maximum]"] != "20" {
		t.Fatalf("session line item = %v, want 5 seats adjustable from 1 to 20", s)
	}
	if c := pay(f.team, ""); c != http.StatusTemporaryRedirect || sessions[len(sessions)-1]["line_items[0][quantity]"] != "1" {
		t.Fatalf("/pay without a quantity = %d %v, want the plan's minimum", c, sessions[len(sessions)-1])
	}
	for _, q := range []string{"?quantity=21", "?quantity=0", "?quantity=abc"} {
		if c := pay(f.team, q); c != http.StatusBadRequest {
			t.Fatalf("/pay%s = %d, want 400", q, c)
		}
	}
	if c := pay(f.solo, "?quantity=2"); c != http.StatusBadRequest {
		t.Fatalf("/pay with 2 on a plan not sold per seat = %d, want 400", c)
	}
	if c := pay(f.solo, ""); c != http.StatusTemporaryRedirect || sessions[len(sessions)-1]["line_items[0][adjustable_quantity][enabled]"] != "" {
		t.Fatalf("/pay of solo = %d %v, want one licence and nothing to adjust", c, sessions[len(sessions)-1])
	}
}

// A paid checkout of a plan sold per seat issues a licence for the
// seats the subscription bills.
func TestFulfill_StoresSubscriptionQuantity(t *testing.T) {
	f := newSeatFixture(t, 1)
	subID := "sub_new_" + f.suffix
	buyer := newFakeSubscription(subID, f.team.StripePriceID, 7, nil)
	f.fake.extra = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/v1/subscriptions/"+subID:
			_, _ = w.Write([]byte(buyer.json(false)))
		case strings.HasPrefix(r.URL.Path, "/v1/customers/"):
			fmt.Fprintf(w, `{"id":%q,"object":"customer","email":"buyer-%s@example.com"}`, buyer.customer, f.suffix)
		default:
			return false
		}
		return true
	}
	ok, err := f.h.fulfillCheckout(f.ctx, "", buyer.customer, subID, "", map[string]string{
		"plan_id": f.team.ID, "session_id": "cs_seats_" + f.suffix, metaLicenseType: "subscription",
	}, "test")
	if err != nil || !ok {
		t.Fatalf("fulfil = %v %v", ok, err)
	}
	l, err := f.s.FindLicenseByStripeSubscription(f.ctx, subID)
	if err != nil {
		t.Fatal(err)
	}
	if l.Quantity != 7 {
		t.Fatalf("quantity of the new licence = %d, want 7", l.Quantity)
	}
}

// An upgrade to a plan sold per seat starts the checkout with a seat for
// everyone already on the licence, takes the seats the subscription
// bills, and gives the old count back if the upgrade is undone.
func TestUpgrade_PerSeat(t *testing.T) {
	f := newUpgradeFixture(t, "upseat")
	if _, err := f.s.DB.NewRaw("UPDATE plans SET per_seat = true, min_quantity = 1, max_quantity = 20 WHERE id = ?", f.paid.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := f.s.CreateSeat(f.ctx, &model.Seat{LicenseID: f.lic.ID, Email: fmt.Sprintf("m%d-%s", i, f.lic.Email), Role: "member"}); err != nil {
			t.Fatal(err)
		}
	}
	subID := "sub_seat_" + f.tag
	sub := newFakeSubscription(subID, f.paid.StripePriceID, 4, nil)
	var sessions []map[string]string
	sub.extra = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/prices/"):
			fmt.Fprintf(w, `{"id":%q,"object":"price","type":"recurring","active":true}`, f.paid.StripePriceID)
		case r.URL.Path == "/v1/checkout/sessions":
			_ = r.ParseForm()
			form := map[string]string{}
			for k, v := range r.PostForm {
				form[k] = v[0]
			}
			sessions = append(sessions, form)
			fmt.Fprintf(w, `{"id":"cs_upseat_start_%s","object":"checkout.session","url":"https://checkout.stripe.test/x"}`, f.tag)
		default:
			return false
		}
		return true
	}
	stubStripe(t, sub.serve(t))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/portal/licenses/upgrade",
		bytes.NewBufferString(fmt.Sprintf(`{"license_id":%q,"plan_id":%q}`, f.lic.ID, f.paid.ID)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("email", f.lic.Email)
	f.h.StartUpgrade(c)
	if w.Code != http.StatusOK || len(sessions) != 1 {
		t.Fatalf("start upgrade = %d %s", w.Code, w.Body.String())
	}
	if s := sessions[0]; s["line_items[0][quantity]"] != "3" || s["line_items[0][adjustable_quantity][enabled]"] != "true" {
		t.Fatalf("upgrade checkout = %v, want 3 seats, adjustable", s)
	}
	// Invites wait while it is open, so an abandoned one lapses soon.
	if exp, _ := strconv.ParseInt(sessions[0]["expires_at"], 10, 64); exp == 0 || time.Until(time.Unix(exp, 0)) > store.PerSeatUpgradeCheckoutTTL+time.Minute {
		t.Fatalf("upgrade checkout expires_at = %q, want within the hour", sessions[0]["expires_at"])
	}

	sessionID := "cs_upseat_" + f.tag
	f.applySub(t, sessionID, subID)
	if l := f.reload(t); l.PlanID != f.paid.ID || l.Quantity != 4 {
		t.Fatalf("after the upgrade: plan %s quantity %d, want the paid plan with 4", l.PlanID, l.Quantity)
	}
	if u := f.upgradeRow(t, sessionID); u.FromQuantity != 1 {
		t.Fatalf("upgrade row remembers %d seats, want 1", u.FromQuantity)
	}
	if err := f.h.onSubscriptionDeleted(f.ctx, fmt.Appendf(nil, `{"id":%q}`, subID)); err != nil {
		t.Fatal(err)
	}
	if l := f.reload(t); l.PlanID != f.free.ID || l.Quantity != 1 {
		t.Fatalf("after the subscription ended: plan %s quantity %d, want the free plan with 1", l.PlanID, l.Quantity)
	}
}

// The same change sent again is the same request to Stripe, which
// answers with what it said the first time. That answer is history: a
// subscription that has moved on since is read again, and the licence
// never takes seats Stripe does not bill.
func TestSeats_ReplayedAnswerIsNotApplied(t *testing.T) {
	f := newSeatFixture(t, 2)
	up := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":10}`, f.lic.ID))
	if c, out := f.change(up, ""); c != http.StatusOK || envData(out)["status"] != "applied" {
		t.Fatalf("up to 10 = %d %v", c, out)
	}
	down := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	if c, out := f.change(down, ""); c != http.StatusOK {
		t.Fatalf("down to 2 = %d %v", c, out)
	}
	f.subscriptionUpdated() // Stripe's event for it
	// The first request again: it started from 2 seats, which is what
	// the subscription has once more, so only Stripe can tell.
	c, out := f.change(up, "")
	if c != http.StatusConflict || envCode(out) != "SUBSCRIPTION_CHANGED" {
		t.Fatalf("the first change replayed = %d %v, want 409 SUBSCRIPTION_CHANGED", c, out)
	}
	if q := f.license().Quantity; q != 2 {
		t.Fatalf("licence holds %d seats after a replay, Stripe bills 2", q)
	}
}

// A retry of a change whose answer never arrived finds it made.
func TestSeats_RetryOfAChangeMadeIsApplied(t *testing.T) {
	f := newSeatFixture(t, 2)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":4}`, f.lic.ID))
	if c, _ := f.change(p, ""); c != http.StatusOK {
		t.Fatal(c)
	}
	// Answer lost: the licence is put back as if nothing came of it.
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET quantity = 2, stripe_synced_at = NULL WHERE id = ?", f.lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	if c, out := f.change(p, ""); c != http.StatusOK || envData(out)["status"] != "applied" {
		t.Fatalf("retry of a change made = %d %v, want applied", c, out)
	}
	if q := f.license().Quantity; q != 4 {
		t.Fatalf("quantity = %d, want 4", q)
	}
}

// A subscription still on a price its plan no longer sells changes seats
// at that price, and is not refused.
func TestSeats_OlderPriceKeepsItsPrice(t *testing.T) {
	f := newSeatFixture(t, 2)
	f.fake.set("price_retired_"+f.suffix, 2)
	f.fake.unit["price_retired_"+f.suffix] = 900
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":3}`, f.lic.ID))
	if cur := p["current"].(map[string]any); cur["plan_id"] != f.team.ID || cur["price_id"] != "price_retired_"+f.suffix {
		t.Fatalf("current = %v, want the licence's plan at the price billed", cur)
	}
	if c, out := f.change(p, ""); c != http.StatusOK {
		t.Fatalf("change = %d %v", c, out)
	}
	f.fake.mu.Lock()
	u := f.fake.updates[len(f.fake.updates)-1]
	f.fake.mu.Unlock()
	if u["items[0][price]"] != "price_retired_"+f.suffix || u["items[0][quantity]"] != "3" {
		t.Fatalf("update = %v, want the older price kept at 3 seats", u)
	}
	if l := f.license(); l.Quantity != 3 || l.PlanID != f.team.ID {
		t.Fatalf("licence: plan %s quantity %d", l.PlanID, l.Quantity)
	}
	// Moving it to another plan works too, unless it is a downgrade: a
	// read of the older price would not show which plan it ended on.
	if c, out := f.post(f.h.ChangePlan, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"new_price_id":%q}`, f.lic.ID, f.solo.StripePriceID)); c != http.StatusConflict || envCode(out) != "DOWNGRADE_NOT_SUPPORTED" {
		t.Fatalf("downgrade to another plan from an older price = %d %v, want DOWNGRADE_NOT_SUPPORTED", c, out)
	}
	big := &model.Plan{ProductID: f.prod.ID, Name: "Bigger", Slug: "bigger-" + f.suffix, LicenseType: "subscription",
		LicenseModel: "standard", Active: true, StripePriceID: "price_bigger_" + f.suffix,
		PerSeat: true, MinQuantity: 1, MaxQuantity: 20, MaxActivations: 4, MaxSeats: 1}
	if err := f.s.CreatePlan(f.ctx, big); err != nil {
		t.Fatal(err)
	}
	f.fake.unit[big.StripePriceID] = 2000
	if c, out := f.post(f.h.ChangePlan, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"new_price_id":%q}`, f.lic.ID, big.StripePriceID)); c != http.StatusOK {
		t.Fatalf("change-plan up from an older price = %d %v", c, out)
	}
}

// A failed invoice for a change leaves a subscription Stripe still has
// active out of dunning, whether or not the change still waits.
func TestSeats_ChangeInvoiceFailureOnActiveSubscription(t *testing.T) {
	f := newSeatFixture(t, 2)
	if err := f.h.onPaymentFailed(f.ctx, fmt.Appendf(nil, `{"subscription":%q,"billing_reason":"subscription_update"}`, f.lic.StripeSubscriptionID)); err != nil {
		t.Fatal(err)
	}
	if l := f.license(); l.Status != model.StatusActive {
		t.Fatalf("status %s, want active", l.Status)
	}
	f.fake.mu.Lock()
	f.fake.status = "past_due"
	f.fake.mu.Unlock()
	if err := f.h.onPaymentFailed(f.ctx, fmt.Appendf(nil, `{"subscription":%q,"billing_reason":"subscription_update"}`, f.lic.StripeSubscriptionID)); err != nil {
		t.Fatal(err)
	}
	if l := f.license(); l.Status != model.StatusPastDue {
		t.Fatalf("a change that put the subscription behind: status %s, want past_due", l.Status)
	}
}

// Refunding a change on a licence that came from a free upgrade gives
// the change back and keeps the upgrade and its subscription.
func TestUpgrade_ChangeRefundKeepsTheUpgrade(t *testing.T) {
	f := newUpgradeFixture(t, "upchref")
	subID := "sub_chref_" + f.tag
	sub := newFakeSubscription(subID, f.paid.StripePriceID, 1, nil)
	canceled := false
	sub.extra = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/v1/invoices/in_chref":
			fmt.Fprintf(w, `{"id":"in_chref","object":"invoice","billing_reason":"subscription_update","lines":{"object":"list","has_more":false,"data":[{"id":"il_1","object":"line_item","amount":500,"parent":{"type":"subscription_item_details","subscription_item_details":{"proration":true}}}]},"parent":{"type":"subscription_details","subscription_details":{"subscription":%q}}}`, subID)
		case r.URL.Path == "/v1/subscriptions/"+subID && r.Method == http.MethodDelete:
			canceled = true
			_, _ = w.Write([]byte(sub.json(false)))
		default:
			return false
		}
		return true
	}
	stubStripe(t, sub.serve(t))
	f.applySub(t, "cs_chref_"+f.tag, subID)
	if err := f.h.onChargeRefunded(f.ctx, []byte(`{"id":"ch_chref","invoice":"in_chref","refunded":true,"amount_refunded":500}`)); err != nil {
		t.Fatal(err)
	}
	if l := f.reload(t); l.PlanID != f.paid.ID || l.StripeSubscriptionID != subID || l.Status != model.StatusActive || canceled {
		t.Fatalf("after refunding a change: plan %s sub %q status %s canceled %v; want the paid plan, still billed", l.PlanID, l.StripeSubscriptionID, l.Status, canceled)
	}
}

// Changes waiting on a slow Stripe hold no database connection: many at
// once leave the pool free for everything else.
func TestSeats_SlowStripeHoldsNoConnection(t *testing.T) {
	f := newSeatFixture(t, 2)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":4}`, f.lic.ID))
	f.fake.delay = 300 * time.Millisecond
	const n = 40 // more than the pool's 25 connections
	done := make(chan int, n)
	for range n {
		go func() {
			c, _ := f.change(p, "")
			done <- c
		}()
	}
	time.Sleep(100 * time.Millisecond)
	// While they wait on Stripe, an unrelated query gets a connection.
	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer cancel()
	var one int
	if err := f.s.DB.NewRaw("SELECT 1").Scan(ctx, &one); err != nil {
		t.Fatalf("a query while changes wait on Stripe: %v", err)
	}
	timeout := time.After(30 * time.Second)
	for range n {
		select {
		case <-done:
		case <-timeout:
			t.Fatal("changes did not finish")
		}
	}
	if q := f.license().Quantity; q != 4 {
		t.Fatalf("quantity = %d, want 4", q)
	}
}

// A newer read puts back the plan Stripe bills even when an older read
// landed in between and this worker's copy of the licence already showed
// that plan: the write carries plan_id whenever the price maps to one.
func TestSeats_NewerReadPutsThePlanBack(t *testing.T) {
	f := newSeatFixture(t, 2)
	copyBefore := f.license() // on team, as Stripe bills
	asOf, err := f.s.StripeReadStamp(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := f.h.currentSubscription(f.lic.StripeSubscriptionID)
	if err != nil || cur == nil {
		t.Fatalf("read: %v", err)
	}
	// An older read (solo) lands first.
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET plan_id = ?, stripe_synced_at = ? WHERE id = ?",
		f.solo.ID, asOf.Add(-time.Minute), f.lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.h.syncFromCurrent(f.ctx, copyBefore, cur, asOf, "test"); err != nil {
		t.Fatal(err)
	}
	if l := f.license(); l.PlanID != f.team.ID {
		t.Fatalf("plan after the newer read = %s, want team (%s)", l.PlanID, f.team.ID)
	}
}

// refundInvoices serves the invoices the refund tests charge against:
// in_change bills a change's difference only; in_newperiod is a change
// that started a new period (monthly to yearly) and bills that period;
// in_cycle is a renewal.
func refundInvoices(subID string) func(w http.ResponseWriter, r *http.Request) bool {
	line := func(proration bool) string {
		return fmt.Sprintf(`{"id":"il_%v","object":"line_item","amount":1000,"parent":{"type":"subscription_item_details","subscription_item_details":{"proration":%v}}}`, proration, proration)
	}
	invoice := func(id, reason string, lines ...string) string {
		return fmt.Sprintf(`{"id":%q,"object":"invoice","billing_reason":%q,"lines":{"object":"list","has_more":false,"data":[%s]},"parent":{"type":"subscription_details","subscription_details":{"subscription":%q}}}`,
			id, reason, strings.Join(lines, ","), subID)
	}
	return func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/v1/invoices/in_change":
			_, _ = w.Write([]byte(invoice("in_change", "subscription_update", line(true), line(true))))
		case "/v1/invoices/in_newperiod":
			_, _ = w.Write([]byte(invoice("in_newperiod", "subscription_update", line(true), line(false))))
		case "/v1/invoices/in_cycle":
			_, _ = w.Write([]byte(invoice("in_cycle", "subscription_cycle", line(false))))
		default:
			return false
		}
		return true
	}
}

// A change that started a new period billed that period: refunding it
// is refunding the subscription, and the licence goes as it always has.
func TestSeats_NewPeriodRefundRevokes(t *testing.T) {
	f := newSeatFixture(t, 2)
	f.fake.extra = refundInvoices(f.lic.StripeSubscriptionID)
	if err := f.h.onChargeRefunded(f.ctx, []byte(`{"id":"ch_np","invoice":"in_newperiod","refunded":true,"amount_refunded":12000}`)); err != nil {
		t.Fatal(err)
	}
	if l := f.license(); l.Status != model.StatusRevoked {
		t.Fatalf("after refunding a new period: status %s, want revoked", l.Status)
	}
}

// Seats bought for a moment do not leave their devices behind: lowering
// the seats below the devices in use waits for the owner to free them.
// Going up while still over a limit the licence was already over is fine.
func TestSeats_DevicesInUseHoldBackALowerLimit(t *testing.T) {
	f := newSeatFixture(t, 5) // 5 seats × 2 devices
	for i := range 7 {
		if err := f.s.CreateActivation(f.ctx, &model.Activation{LicenseID: f.lic.ID, Identifier: fmt.Sprintf("dev-%d-%s", i, f.suffix), IdentifierType: "device"}); err != nil {
			t.Fatal(err)
		}
	}
	c, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"quantity":3}`, f.lic.ID))
	if c != http.StatusConflict || envCode(out) != "DEVICES_IN_USE" {
		t.Fatalf("down to 3 seats (6 devices) with 7 activated = %d %v, want 409 DEVICES_IN_USE", c, out)
	}
	if c, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"quantity":4}`, f.lic.ID)); c != http.StatusOK {
		t.Fatalf("down to 4 seats (8 devices) with 7 activated = %d %v, want allowed", c, out)
	}
	// Over a limit set from Stripe (2 seats, 4 devices), going up to 3
	// (6 devices) is still over, but no further: allowed.
	f.fake.set(f.team.StripePriceID, 2)
	f.subscriptionUpdated()
	if c, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"quantity":3}`, f.lic.ID)); c != http.StatusOK {
		t.Fatalf("up from 2 to 3 seats while over = %d %v, want allowed", c, out)
	}
}

// A downgrade is not prorated: nothing is invoiced now, and the next
// invoice bills the new terms. More seats are prorated from the moment
// they were shown.
func TestSeats_DowngradeIsNotProrated(t *testing.T) {
	f := newSeatFixture(t, 5)
	down := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	if c, out := f.change(down, ""); c != http.StatusOK || envData(out)["status"] != "applied" {
		t.Fatalf("downgrade = %d %v", c, out)
	}
	up := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":4}`, f.lic.ID))
	tok, _ := f.h.openChange(up["preview_token"].(string))
	shown := time.Now().Add(-50 * time.Minute).Unix()
	tok.ProrationDate = shown
	if c, out := f.change(map[string]any{"preview_token": f.h.signChange(tok)}, ""); c != http.StatusOK {
		t.Fatalf("charge = %d %v", c, out)
	}
	f.fake.mu.Lock()
	defer f.fake.mu.Unlock()
	downgrade, charge := f.fake.updates[0], f.fake.updates[1]
	if downgrade["proration_behavior"] != "none" || downgrade["proration_date"] != "" || downgrade["payment_behavior"] != "" {
		t.Fatalf("downgrade sent %v, want proration none and nothing to pay", downgrade)
	}
	if f.fake.balance != 0 {
		t.Fatalf("balance after a downgrade = %d, want nothing credited", f.fake.balance)
	}
	if charge["proration_behavior"] != "always_invoice" || charge["payment_behavior"] != "pending_if_incomplete" || charge["proration_date"] != fmt.Sprint(shown) {
		t.Fatalf("charge sent %v, want always_invoice, held for payment, from the preview's %d", charge, shown)
	}
}

// Stripe's 409 (the same request still being handled) and 429 say
// nothing about whether the change was made: processing, not refused.
func TestSeats_ConflictFromStripeIsUnknown(t *testing.T) {
	f := newSeatFixture(t, 2)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":4}`, f.lic.ID))
	f.fake.updateErr = http.StatusConflict
	if c, out := f.change(p, ""); c != http.StatusAccepted || envData(out)["status"] != "processing" {
		t.Fatalf("Stripe 409 = %d %v, want 202 processing", c, out)
	}
}

// A subscription the seller gave a second item reads its seats from the
// item a plan of the product sells, wherever it sits.
func TestSeats_SyncReadsTheProductItem(t *testing.T) {
	f := newSeatFixture(t, 2)
	addonPrice := "price_addon_" + f.suffix
	f.fake.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1/subscriptions/"+f.lic.StripeSubscriptionID || r.Method != http.MethodGet {
			return false
		}
		fmt.Fprintf(w, `{"id":%q,"object":"subscription","status":"active","customer":%q,"collection_method":"charge_automatically",
			"items":{"object":"list","data":[
			{"id":"si_addon","object":"subscription_item","quantity":1,"price":{"id":%q,"object":"price"}},
			{"id":"si_team","object":"subscription_item","quantity":5,"price":{"id":%q,"object":"price"}}]}}`,
			f.lic.StripeSubscriptionID, f.fake.customer, addonPrice, f.team.StripePriceID)
		return true
	}
	// The extra handler answers before the fake: route the read to it.
	f.fake.id = "sub_elsewhere_" + f.suffix
	f.subscriptionUpdated()
	if l := f.license(); l.Quantity != 5 || l.PlanID != f.team.ID {
		t.Fatalf("after a read with an add-on first: quantity %d plan %s, want 5 on team", l.Quantity, l.PlanID)
	}
}

// A team larger than a per-seat plan sells cannot upgrade to it: the
// licence would be over its limit from the first day.
func TestUpgrade_PerSeatTeamTooLarge(t *testing.T) {
	f := newUpgradeFixture(t, "upbig")
	if _, err := f.s.DB.NewRaw("UPDATE plans SET per_seat = true, min_quantity = 1, max_quantity = 2 WHERE id = ?", f.paid.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := f.s.CreateSeat(f.ctx, &model.Seat{LicenseID: f.lic.ID, Email: fmt.Sprintf("m%d-%s", i, f.lic.Email), Role: "member"}); err != nil {
			t.Fatal(err)
		}
	}
	sub := newFakeSubscription("sub_unused_"+f.tag, f.paid.StripePriceID, 1, nil)
	sub.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/v1/prices/") {
			fmt.Fprintf(w, `{"id":%q,"object":"price","type":"recurring","active":true}`, f.paid.StripePriceID)
			return true
		}
		return false
	}
	stubStripe(t, sub.serve(t))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/portal/licenses/upgrade",
		bytes.NewBufferString(fmt.Sprintf(`{"license_id":%q,"plan_id":%q}`, f.lic.ID, f.paid.ID)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("email", f.lic.Email)
	f.h.StartUpgrade(c)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "SEATS_IN_USE") {
		t.Fatalf("upgrade of 3 people to a plan of up to 2 = %d %s, want 409 SEATS_IN_USE", w.Code, w.Body.String())
	}
}

// change-plan sent again after its answer was lost finds the change made.
func TestChangePlan_RetryFindsItMade(t *testing.T) {
	f := newSeatFixture(t, 3)
	body := fmt.Sprintf(`{"license_id":%q,"new_price_id":%q}`, f.lic.ID, f.big.StripePriceID)
	f.fake.set(f.team.StripePriceID, 10)
	if c, out := f.post(f.h.ChangePlan, f.lic.Email, body); c != http.StatusOK {
		t.Fatalf("first = %d %v", c, out)
	}
	if c, out := f.post(f.h.ChangePlan, f.lic.Email, body); c != http.StatusOK || envData(out)["status"] != "plan_changed" {
		t.Fatalf("again = %d %v, want plan_changed", c, out)
	}
}

// A change waiting for payment is dropped by asking for what the
// subscription has now: nothing to pay, nothing changes, nothing waits.
func TestSeats_DropAHeldChange(t *testing.T) {
	f := newSeatFixture(t, 2)
	f.fake.failPayment = true
	if c, out := f.change(f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":6}`, f.lic.ID)), ""); c != http.StatusOK || envData(out)["status"] != "pending_payment" {
		t.Fatalf("held change = %d %v", c, out)
	}
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	if p["total"] != float64(0) || p["replaces_pending"] != true {
		t.Fatalf("preview of the seats it has = total %v replaces %v, want 0 and true", p["total"], p["replaces_pending"])
	}
	if c, out := f.change(p, ""); c != http.StatusOK || envData(out)["status"] != "applied" {
		t.Fatalf("drop = %d %v, want applied", c, out)
	}
	if l := f.license(); l.Quantity != 2 || l.StripePendingUpdateExpiresAt != nil {
		t.Fatalf("after dropping: quantity %d pending %v, want 2 and none", l.Quantity, l.StripePendingUpdateExpiresAt)
	}
	// With nothing waiting, the same ask is refused as no change.
	if c, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID)); c != http.StatusConflict || envCode(out) != "NO_CHANGE" {
		t.Fatalf("preview of no change = %d %v, want 409 NO_CHANGE", c, out)
	}
}

// What the customer owes goes on the invoice too, and Stripe saying no
// to a preview is a refusal, not a server error.
func TestSeats_PreviewOwedBalanceAndRefusal(t *testing.T) {
	f := newSeatFixture(t, 2)
	f.fake.balance = 700
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":4}`, f.lic.ID))
	if p["total"] != float64(1000) || p["due_now"] != float64(1700) || p["credit_balance"] != float64(0) {
		t.Fatalf("owed: total %v due %v credit %v, want 1000, 1700, 0", p["total"], p["due_now"], p["credit_balance"])
	}
	f.fake.previewErr = http.StatusBadRequest
	if c, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"quantity":5}`, f.lic.ID)); c != http.StatusConflict || envCode(out) != "CHANGE_REFUSED" {
		t.Fatalf("preview Stripe refused = %d %v, want 409 CHANGE_REFUSED", c, out)
	}
}

// While a downgrade is with Stripe, nothing is taken from the licence's
// limits: activations racing it wait, and then meet the lower limit.
func TestSeats_DowngradeHoldsActivationsBack(t *testing.T) {
	f := newSeatFixture(t, 10) // 10 seats × 2 = 20 devices
	activate := func(n, max int) error {
		return f.s.ActivateWithinLimit(f.ctx, &model.Activation{LicenseID: f.lic.ID, Identifier: fmt.Sprintf("race-%d-%s", n, f.suffix), IdentifierType: "device"}, max)
	}
	for i := range 2 {
		if err := activate(i, 20); err != nil {
			t.Fatal(err)
		}
	}
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":1}`, f.lic.ID)) // 2 devices
	f.fake.delay = 400 * time.Millisecond
	done := make(chan int, 1)
	go func() {
		c, _ := f.change(p, "")
		done <- c
	}()
	time.Sleep(150 * time.Millisecond)
	let := 0
	for i := 2; i < 12; i++ {
		// The caller read the 20 devices the licence had before.
		if activate(i, 20) == nil {
			let++
		}
	}
	if c := <-done; c != http.StatusOK {
		t.Fatalf("change = %d", c)
	}
	if let != 0 {
		t.Fatalf("%d activations got in while the change was with Stripe, want 0", let)
	}
	if q := f.license().Quantity; q != 1 {
		t.Fatalf("quantity = %d, want 1", q)
	}
}

// Subscriptions that cannot hold a change for payment, or whose
// collection is paused, are not changed here; a preview from before a
// renewal is out of date.
func TestSeats_MoreRefusals(t *testing.T) {
	f := newSeatFixture(t, 2)
	body := fmt.Sprintf(`{"license_id":%q,"quantity":3}`, f.lic.ID)
	f.fake.pmType = "sepa_debit"
	if c, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, body); c != http.StatusConflict || envCode(out) != "PAYMENT_METHOD_UNSUPPORTED" {
		t.Fatalf("bank debit = %d %v, want PAYMENT_METHOD_UNSUPPORTED", c, out)
	}
	f.fake.pmType = "card"
	f.fake.paused = true
	if c, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, body); c != http.StatusConflict || envCode(out) != "CHANGE_NOT_SUPPORTED" {
		t.Fatalf("paused collection = %d %v, want CHANGE_NOT_SUPPORTED", c, out)
	}
	f.fake.paused = false
	p := f.preview(body)
	f.fake.mu.Lock()
	f.fake.periodStart = time.Now().Add(time.Minute).Unix() // renewed since the preview
	f.fake.mu.Unlock()
	if c, out := f.change(p, ""); c != http.StatusConflict || envCode(out) != "PREVIEW_EXPIRED" {
		t.Fatalf("preview from the last period = %d %v, want PREVIEW_EXPIRED", c, out)
	}
}

// One change at a time is with Stripe: a second, sent while a downgrade
// holds the licence's seats, is told to wait.
func TestSeats_OneChangeAtATime(t *testing.T) {
	f := newSeatFixture(t, 6)
	a := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":4}`, f.lic.ID))
	b := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":3}`, f.lic.ID))
	f.fake.delay = 400 * time.Millisecond
	done := make(chan int, 1)
	go func() {
		c, _ := f.change(a, "")
		done <- c
	}()
	time.Sleep(150 * time.Millisecond)
	// The first downgrade holds the licence's seats: the second waits.
	if c, out := f.change(b, ""); c != http.StatusConflict || envCode(out) != "CHANGE_IN_PROGRESS" {
		t.Fatalf("a second change while one is with Stripe = %d %v, want 409 CHANGE_IN_PROGRESS", c, out)
	}
	if c := <-done; c != http.StatusOK {
		t.Fatalf("first = %d", c)
	}
	if q := f.license().Quantity; q != 4 {
		t.Fatalf("quantity = %d, want 4", q)
	}
}

// Moving between plans not sold per seat keeps a quantity the seller set
// in Stripe; nothing is credited back that was not asked for.
func TestSeats_PlanChangeKeepsASellerSetQuantity(t *testing.T) {
	f := newSeatFixture(t, 3)
	other := &model.Plan{ProductID: f.prod.ID, Name: "Other", Slug: "other-" + f.suffix, LicenseType: "subscription",
		LicenseModel: "standard", Active: true, StripePriceID: "price_other_" + f.suffix, MaxActivations: 3, MaxSeats: 1}
	if err := f.s.CreatePlan(f.ctx, other); err != nil {
		t.Fatal(err)
	}
	f.fake.unit[other.StripePriceID] = 700
	f.fake.set(f.solo.StripePriceID, 3) // non per seat, 3 units set in Stripe
	f.subscriptionUpdated()
	c, out := f.post(f.h.ChangePlan, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"new_price_id":%q}`, f.lic.ID, other.StripePriceID))
	if c != http.StatusOK {
		t.Fatalf("change-plan = %d %v", c, out)
	}
	f.fake.mu.Lock()
	u := f.fake.updates[len(f.fake.updates)-1]
	f.fake.mu.Unlock()
	if u["items[0][quantity]"] != "3" {
		t.Fatalf("quantity sent = %s, want the 3 the seller set", u["items[0][quantity]"])
	}
}

// More seats asked while a downgrade is with Stripe wait for it, and are
// then checked against what it left.
func TestSeats_IncreaseWaitsForADowngrade(t *testing.T) {
	f := newSeatFixture(t, 6)
	down := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":3}`, f.lic.ID))
	up := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":8}`, f.lic.ID))
	f.fake.delay = 400 * time.Millisecond
	done := make(chan int, 1)
	go func() {
		c, _ := f.change(down, "")
		done <- c
	}()
	time.Sleep(150 * time.Millisecond)
	if c, out := f.change(up, ""); c != http.StatusConflict || envCode(out) != "CHANGE_IN_PROGRESS" {
		t.Fatalf("more seats while a downgrade is with Stripe = %d %v, want 409 CHANGE_IN_PROGRESS", c, out)
	}
	<-done
}

// A downgrade Stripe gave no answer for, and did not make, can be sent
// again.
func TestSeats_UnansweredDowngradeCanBeSentAgain(t *testing.T) {
	f := newSeatFixture(t, 5)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	f.fake.updateErr = http.StatusServiceUnavailable
	if c, out := f.change(p, ""); c != http.StatusAccepted {
		t.Fatalf("unanswered = %d %v, want 202", c, out)
	}
	f.fake.updateErr = 0
	if c, out := f.change(p, ""); c != http.StatusOK || envData(out)["status"] != "applied" {
		t.Fatalf("the same change again = %d %v, want applied", c, out)
	}
	if q := f.license().Quantity; q != 2 {
		t.Fatalf("quantity = %d, want 2", q)
	}
}

// Stripe answering 429 each time may hide an attempt the client retried
// that Stripe is still running: the outcome is unknown, and a downgrade
// keeps its lower seats until Stripe shows how it went.
func TestSeats_StripeBusyIsUnknown(t *testing.T) {
	f := newSeatFixture(t, 5)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	f.fake.updateErr = http.StatusTooManyRequests
	if c, out := f.change(p, ""); c != http.StatusAccepted || envData(out)["status"] != "processing" {
		t.Fatalf("Stripe busy = %d %v, want 202 processing", c, out)
	}
	if q := f.license().Quantity; q != 2 {
		t.Fatalf("quantity = %d, want the lower 2 kept", q)
	}
	// While Stripe may still make it, a look at the seats keeps the lower
	// ones; once the hold is over, the next look brings the licence back
	// in line with what Stripe bills.
	f.fake.updateErr = 0
	f.post(f.h.PreviewSubscriptionChange, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"quantity":4}`, f.lic.ID))
	if q := f.license().Quantity; q != 2 {
		t.Fatalf("quantity after a preview while held = %d, want the lower 2", q)
	}
	f.endHold()
	f.post(f.h.PreviewSubscriptionChange, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"quantity":4}`, f.lic.ID))
	if q := f.license().Quantity; q != 5 {
		t.Fatalf("quantity after a preview = %d, want the 5 Stripe bills", q)
	}
}

// A downgrade sent again after the subscription moved on changes
// nothing.
func TestSeats_ReplayedDowngradeChangesNothing(t *testing.T) {
	f := newSeatFixture(t, 5)
	down := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	if c, _ := f.change(down, ""); c != http.StatusOK {
		t.Fatal(c)
	}
	f.subscriptionUpdated()
	up := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":5}`, f.lic.ID))
	if c, _ := f.change(up, ""); c != http.StatusOK {
		t.Fatal(c)
	}
	f.subscriptionUpdated()
	if c, out := f.change(down, ""); c != http.StatusConflict || envCode(out) != "SUBSCRIPTION_CHANGED" {
		t.Fatalf("the lowering change again = %d %v, want 409 SUBSCRIPTION_CHANGED", c, out)
	}
	more := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":6}`, f.lic.ID))
	if c, out := f.change(more, ""); c != http.StatusOK {
		t.Fatalf("more seats right after = %d %v", c, out)
	}
}

// A change previewed before the subscription renewed is refused, credit
// or charge.
func TestSeats_CreditPreviewedBeforeRenewalIsRefused(t *testing.T) {
	f := newSeatFixture(t, 5)
	down := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	tok, _ := f.h.openChange(down["preview_token"].(string))
	tok.ProrationDate = time.Now().Add(-30 * time.Minute).Unix()
	f.fake.mu.Lock()
	f.fake.periodStart = time.Now().Add(-10 * time.Minute).Unix()
	f.fake.mu.Unlock()
	if c, out := f.change(map[string]any{"preview_token": f.h.signChange(tok)}, ""); c != http.StatusConflict || envCode(out) != "PREVIEW_EXPIRED" {
		t.Fatalf("credit previewed before renewal = %d %v, want 409 PREVIEW_EXPIRED", c, out)
	}
}

// Two changes that raise the seats, checked on the same subscription,
// are not both with Stripe: the second waits, and checked again it is
// refused when the first leaves more people than it pays for.
func TestSeats_TwoRaisesOneAtATime(t *testing.T) {
	f := newSeatFixture(t, 5)
	eight := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":8}`, f.lic.ID))
	six := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":6}`, f.lic.ID))
	f.fake.delay = 400 * time.Millisecond
	done := make(chan int, 1)
	go func() {
		c, _ := f.change(eight, "")
		done <- c
	}()
	time.Sleep(150 * time.Millisecond)
	if c, out := f.change(six, ""); c != http.StatusConflict || envCode(out) != "SUBSCRIPTION_CHANGED" {
		t.Fatalf("a second raise while one is with Stripe = %d %v, want 409 SUBSCRIPTION_CHANGED once the first is made", c, out)
	}
	if c := <-done; c != http.StatusOK {
		t.Fatalf("first = %d", c)
	}
	f.fake.delay = 0
	for i := 0; i < 7; i++ { // 8 people with the owner
		if _, _, err := f.s.AddSeatWithinLimit(f.ctx, &model.Seat{LicenseID: f.lic.ID, Email: fmt.Sprintf("r%d-%s", i, f.lic.Email), Role: "member"}, 8); err != nil {
			t.Fatalf("person %d on 8 seats: %v", i, err)
		}
	}
	// The second, sent again: the subscription moved on.
	if c, out := f.change(six, ""); c != http.StatusConflict {
		t.Fatalf("the second raise after the first = %d %v, want refused", c, out)
	}
	if q := f.license().Quantity; q != 8 {
		t.Fatalf("quantity = %d, want 8", q)
	}
}

// A change checked against the subscription as it was, made after
// another change raised the seats and more people joined, is checked
// again under the lock against the licence as it is: going to fewer
// seats than are in use now is refused.
func TestSeats_ChangeCheckedAgainstLimitsMovedSince(t *testing.T) {
	f := newSeatFixture(t, 5)
	six := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":6}`, f.lic.ID))
	f.fake.afterGet = func() {
		// Another change lands: 8 seats, and 8 people on them.
		f.fake.set(f.team.StripePriceID, 8)
		f.subscriptionUpdated()
		for i := 0; i < 7; i++ {
			if _, _, err := f.s.AddSeatWithinLimit(f.ctx, &model.Seat{LicenseID: f.lic.ID, Email: fmt.Sprintf("m%d-%s", i, f.lic.Email), Role: "member"}, 8); err != nil {
				t.Errorf("person %d: %v", i, err)
			}
		}
	}
	if c, out := f.change(six, ""); c != http.StatusConflict || envCode(out) != "SEATS_IN_USE" {
		t.Fatalf("6 seats with 8 people on 8 = %d %v, want 409 SEATS_IN_USE", c, out)
	}
	if q := f.license().Quantity; q != 8 {
		t.Fatalf("quantity = %d, want 8", q)
	}
}

// The same key with other parameters is Stripe refusing the request:
// nothing is made, and the customer reviews the change again.
func TestSeats_IdempotencyErrorIsARefusal(t *testing.T) {
	f := newSeatFixture(t, 5)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	tok, _ := f.h.openChange(p["preview_token"].(string))
	f.fake.mu.Lock()
	f.fake.sent = map[string]string{fmt.Sprintf("keygate-change:%s:%s:%d:%d", f.lic.ID, f.team.StripePriceID, 2, tok.ProrationDate): "other"}
	f.fake.replies = map[string]string{}
	f.fake.mu.Unlock()
	if c, out := f.change(p, ""); c != http.StatusConflict || envCode(out) != "CHANGE_REFUSED" {
		t.Fatalf("idempotency error = %d %v, want 409 CHANGE_REFUSED", c, out)
	}
}

// Stripe busy pricing a change is a wait, not a refusal.
func TestSeats_PreviewStripeBusy(t *testing.T) {
	f := newSeatFixture(t, 2)
	f.fake.previewErr = http.StatusTooManyRequests
	c, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"quantity":3}`, f.lic.ID))
	if c != http.StatusTooManyRequests || envCode(out) != "RATE_LIMITED" {
		t.Fatalf("preview while Stripe is busy = %d %v, want 429 RATE_LIMITED", c, out)
	}
}

// Stripe made the change on a first attempt whose answer was lost, and
// the retry was rate limited: the 429 is not taken for "nothing made",
// and the licence takes the lower seats at once.
func TestSeats_BusyAfterAnAttemptThatWasMade(t *testing.T) {
	f := newSeatFixture(t, 5)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	inner := f.fake.serve(t)
	var posts int32
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/subscriptions/"+f.fake.id {
			if atomic.AddInt32(&posts, 1) == 1 {
				inner(httptest.NewRecorder(), r) // made
				conn, _, _ := w.(http.Hijacker).Hijack()
				_ = conn.Close() // and the answer lost
				return
			}
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"rate_limit","message":"Too many requests"}}`))
			return
		}
		inner(w, r)
	})
	if c, out := f.change(p, ""); c != http.StatusOK || envData(out)["status"] != "applied" {
		t.Fatalf("change = %d %v, want applied, as the subscription shows", c, out)
	}
	if q := f.license().Quantity; q != 2 {
		t.Fatalf("quantity = %d, want the 2 Stripe bills", q)
	}
	for i := 0; i < 2; i++ {
		_, _, _ = f.s.AddSeatWithinLimit(f.ctx, &model.Seat{LicenseID: f.lic.ID, Email: fmt.Sprintf("p%d-%s", i, f.lic.Email), Role: "member"}, 5)
	}
	if n, _ := f.s.CountActiveSeats(f.ctx, f.lic.ID); n+1 > 2 {
		t.Fatalf("%d people on a licence Stripe bills 2 seats for", n+1)
	}
}

// Dropping a held change while Stripe is busy is not taken for done: the
// subscription already bills those terms, but the held change is still
// there, so the outcome is unknown.
func TestSeats_DropWhileStripeBusy(t *testing.T) {
	f := newSeatFixture(t, 5)
	f.fake.hold(f.team.StripePriceID, 8)
	f.subscriptionUpdated()
	keep := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":5}`, f.lic.ID))
	f.fake.updateErr = http.StatusTooManyRequests
	if c, out := f.change(keep, ""); c != http.StatusAccepted || envData(out)["status"] != "processing" {
		t.Fatalf("drop while Stripe is busy = %d %v, want 202 processing", c, out)
	}
}

// A licence an operator suspended still takes the seats Stripe bills, so
// it does not come back from the suspension with seats nobody pays for.
func TestSeats_SuspendedLicenceTakesTheBilledSeats(t *testing.T) {
	f := newSeatFixture(t, 5)
	if err := f.s.SuspendLicense(f.ctx, f.lic.ID); err != nil {
		t.Fatal(err)
	}
	f.fake.set(f.team.StripePriceID, 2)
	f.subscriptionUpdated()
	if _, err := f.s.ReinstateLicense(f.ctx, f.lic.ID); err != nil {
		t.Fatal(err)
	}
	if l := f.license(); l.Quantity != 2 {
		t.Fatalf("quantity after the suspension = %d, want the 2 Stripe bills", l.Quantity)
	}
}

// The same request sent again while the first is still with Stripe waits
// for it, and finds it made.
func TestSeats_SameRequestWaitsWhileTheFirstIsWithStripe(t *testing.T) {
	f := newSeatFixture(t, 5)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	f.fake.delay = 400 * time.Millisecond
	done := make(chan int, 1)
	go func() {
		c, _ := f.change(p, "")
		done <- c
	}()
	time.Sleep(150 * time.Millisecond)
	if c, out := f.change(p, ""); c != http.StatusOK || envData(out)["status"] != "applied" {
		t.Fatalf("the same request while the first is with Stripe = %d %v, want applied", c, out)
	}
	if c := <-done; c != http.StatusOK {
		t.Fatalf("first = %d", c)
	}
}

// A downgrade invoices nothing, so it needs no payment method that can
// hold a change: a subscription paid by bank debit can go down, not up.
func TestSeats_BankDebitCanDowngrade(t *testing.T) {
	f := newSeatFixture(t, 5)
	f.fake.pmType = "sepa_debit"
	if c, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"quantity":6}`, f.lic.ID)); c != http.StatusConflict || envCode(out) != "PAYMENT_METHOD_UNSUPPORTED" {
		t.Fatalf("more seats by bank debit = %d %v, want PAYMENT_METHOD_UNSUPPORTED", c, out)
	}
	down := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":3}`, f.lic.ID))
	if c, out := f.change(down, ""); c != http.StatusOK || envData(out)["status"] != "applied" {
		t.Fatalf("fewer seats by bank debit = %d %v, want applied", c, out)
	}
	if q := f.license().Quantity; q != 3 {
		t.Fatalf("quantity = %d, want 3", q)
	}
}

// A plan that lowers a limit is a downgrade, billed from the next
// invoice, only when it costs no more: a dearer one, unprorated, would
// be had for nothing and credited back at its price on the way back.
func TestSeats_DearerPlanWithFewerDevicesIsRefused(t *testing.T) {
	f := newSeatFixture(t, 3) // 3 seats × 2 devices
	mk := func(slug string, unit int64) *model.Plan {
		p := &model.Plan{ProductID: f.prod.ID, Name: slug, Slug: slug + "-" + f.suffix, LicenseType: "subscription",
			LicenseModel: "standard", Active: true, StripePriceID: "price_" + slug + "_" + f.suffix,
			PerSeat: true, MinQuantity: 1, MaxQuantity: 20, MaxActivations: 1, MaxSeats: 1}
		if err := f.s.CreatePlan(f.ctx, p); err != nil {
			t.Fatal(err)
		}
		f.fake.unit[p.StripePriceID] = unit
		return p
	}
	pricey, cheap := mk("pricey", 3000), mk("cheap", 500)
	body := fmt.Sprintf(`{"license_id":%q,"price_id":%q,"quantity":3}`, f.lic.ID, pricey.StripePriceID)
	if c, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, body); c != http.StatusConflict || envCode(out) != "DOWNGRADE_NOT_SUPPORTED" {
		t.Fatalf("preview of a dearer plan with fewer devices = %d %v, want DOWNGRADE_NOT_SUPPORTED", c, out)
	}
	if c, out := f.post(f.h.ChangePlan, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"new_price_id":%q}`, f.lic.ID, pricey.StripePriceID)); c != http.StatusConflict || envCode(out) != "DOWNGRADE_NOT_SUPPORTED" {
		t.Fatalf("change-plan to it = %d %v, want DOWNGRADE_NOT_SUPPORTED", c, out)
	}
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"price_id":%q,"quantity":3}`, f.lic.ID, cheap.StripePriceID))
	if p["downgrade"] != true {
		t.Fatalf("preview of a cheaper plan with fewer devices = %v, want a downgrade", p)
	}
	if c, out := f.change(p, ""); c != http.StatusOK || envData(out)["status"] != "applied" {
		t.Fatalf("change = %d %v, want applied", c, out)
	}
	if l := f.license(); l.PlanID != cheap.ID {
		t.Fatalf("plan = %s, want cheap", l.PlanID)
	}
}

// A downgrade replaces more seats still waiting for payment.
func TestSeats_DowngradeReplacesAHeldIncrease(t *testing.T) {
	f := newSeatFixture(t, 5)
	f.fake.failPayment = true
	up := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":8}`, f.lic.ID))
	if c, out := f.change(up, ""); c != http.StatusOK || envData(out)["status"] != "pending_payment" {
		t.Fatalf("held increase = %d %v", c, out)
	}
	down := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":3}`, f.lic.ID))
	if down["replaces_pending"] != true {
		t.Fatalf("preview = %v, want it to replace the held increase", down)
	}
	if c, out := f.change(down, ""); c != http.StatusOK || envData(out)["status"] != "applied" {
		t.Fatalf("downgrade = %d %v, want applied", c, out)
	}
	if l := f.license(); l.Quantity != 3 || l.StripePendingUpdateExpiresAt != nil {
		t.Fatalf("licence: quantity %d pending %v, want 3 and nothing waiting", l.Quantity, l.StripePendingUpdateExpiresAt)
	}
}

// While a downgrade is with Stripe, invites wait for it, and then meet
// the lower seats.
func TestSeats_DowngradeHoldsInvitesBack(t *testing.T) {
	f := newSeatFixture(t, 6)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	f.fake.delay = 400 * time.Millisecond
	done := make(chan int, 1)
	go func() {
		c, _ := f.change(p, "")
		done <- c
	}()
	time.Sleep(150 * time.Millisecond)
	got := 0
	for i := 0; i < 4; i++ {
		if _, _, err := f.s.AddSeatWithinLimit(f.ctx, &model.Seat{LicenseID: f.lic.ID, Email: fmt.Sprintf("w%d-%s", i, f.lic.Email), Role: "member"}, 6); err == nil {
			got++
		}
	}
	if c := <-done; c != http.StatusOK {
		t.Fatalf("downgrade = %d", c)
	}
	if got != 1 {
		t.Fatalf("%d invites got in during and after a downgrade to 2 seats, want 1", got)
	}
}

// A preview shown as one kind of change (charged now, or a downgrade
// billed from the next invoice) is not made as the other: the plan's
// limits changed in between, and the customer looks again.
func TestSeats_PreviewOfAnotherKindIsOutOfDate(t *testing.T) {
	f := newSeatFixture(t, 3)
	other := &model.Plan{ProductID: f.prod.ID, Name: "Other", Slug: "kind-" + f.suffix, LicenseType: "subscription",
		LicenseModel: "standard", Active: true, StripePriceID: "price_kind_" + f.suffix,
		PerSeat: true, MinQuantity: 1, MaxQuantity: 20, MaxActivations: 2, MaxSeats: 1}
	if err := f.s.CreatePlan(f.ctx, other); err != nil {
		t.Fatal(err)
	}
	f.fake.unit[other.StripePriceID] = 1500
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"price_id":%q,"quantity":3}`, f.lic.ID, other.StripePriceID))
	if p["downgrade"] != false {
		t.Fatalf("preview = %v, want a change charged now", p)
	}
	other.MaxActivations = 1 // now it lowers the devices
	if err := f.s.UpdatePlan(f.ctx, other); err != nil {
		t.Fatal(err)
	}
	if c, out := f.change(p, ""); c != http.StatusConflict || envCode(out) != "PREVIEW_EXPIRED" {
		t.Fatalf("change = %d %v, want 409 PREVIEW_EXPIRED", c, out)
	}
}

// A downgrade to a price billed on another schedule is refused: Stripe
// would start the new schedule, and invoice it, on the day.
func TestSeats_DowngradeToAnotherScheduleIsRefused(t *testing.T) {
	f := newSeatFixture(t, 3)
	yearly := &model.Plan{ProductID: f.prod.ID, Name: "Yearly", Slug: "yearly-" + f.suffix, LicenseType: "subscription",
		LicenseModel: "standard", Active: true, StripePriceID: "price_yearly_" + f.suffix,
		PerSeat: true, MinQuantity: 1, MaxQuantity: 20, MaxActivations: 1, MaxSeats: 1}
	if err := f.s.CreatePlan(f.ctx, yearly); err != nil {
		t.Fatal(err)
	}
	f.fake.unit[yearly.StripePriceID] = 100
	f.fake.interval = map[string]string{yearly.StripePriceID: "year"}
	body := fmt.Sprintf(`{"license_id":%q,"price_id":%q,"quantity":3}`, f.lic.ID, yearly.StripePriceID)
	if c, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, body); c != http.StatusConflict || envCode(out) != "DOWNGRADE_NOT_SUPPORTED" {
		t.Fatalf("downgrade to a yearly price = %d %v, want DOWNGRADE_NOT_SUPPORTED", c, out)
	}
}

// A subscription set to end has no next invoice to show for a downgrade.
func TestSeats_DowngradeOfAnEndingSubscription(t *testing.T) {
	f := newSeatFixture(t, 5)
	f.fake.ending = true
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":3}`, f.lic.ID))
	if p["downgrade"] != true || p["next_invoice_total"] != nil || p["next_invoice_at"] != nil {
		t.Fatalf("preview = %v, want a downgrade with no next invoice", p)
	}
	if c, out := f.change(p, ""); c != http.StatusOK || envData(out)["status"] != "applied" {
		t.Fatalf("change = %d %v, want applied", c, out)
	}
}

// A downgrade waiting on a slow Stripe holds nothing up: activations on
// the licence go on (against its lower limits), each at once, and the
// pool stays free.
func TestSeats_SlowDowngradeHoldsNothingUp(t *testing.T) {
	f := newSeatFixture(t, 40)                                               // 40 seats × 2 = 80 devices
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":20}`, f.lic.ID)) // 40 devices
	f.fake.delay = 2 * time.Second
	done := make(chan int, 1)
	go func() {
		c, _ := f.change(p, "")
		done <- c
	}()
	time.Sleep(200 * time.Millisecond)
	const n = 60
	errs := make(chan error, n)
	start := time.Now()
	for i := range n {
		go func() {
			errs <- f.s.ActivateWithinLimit(f.ctx, &model.Activation{LicenseID: f.lic.ID, Identifier: fmt.Sprintf("slow-%d-%s", i, f.suffix), IdentifierType: "device"}, 80)
		}()
	}
	got := 0
	for range n {
		if <-errs == nil {
			got++
		}
	}
	if time.Since(start) > time.Second {
		t.Fatalf("activations took %v while a downgrade waited on Stripe, want no wait", time.Since(start))
	}
	if got != 40 {
		t.Fatalf("%d activations got in, want the 40 the downgrade leaves", got)
	}
	if c := <-done; c != http.StatusOK {
		t.Fatalf("downgrade = %d", c)
	}
}

// A downgrade Stripe refuses leaves the licence on what Stripe bills.
func TestSeats_RefusedDowngradePutsTheTermsBack(t *testing.T) {
	f := newSeatFixture(t, 5)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	f.fake.updateErr = http.StatusBadRequest
	if c, out := f.change(p, ""); c != http.StatusConflict || envCode(out) != "CHANGE_REFUSED" {
		t.Fatalf("refused downgrade = %d %v", c, out)
	}
	if q := f.license().Quantity; q != 5 {
		t.Fatalf("quantity after a refused downgrade = %d, want 5", q)
	}
}

// A downgrade is announced once: the licence took the new seats before
// Stripe was asked, and the sync that follows finds nothing new.
func TestSeats_DowngradeAnnouncedOnce(t *testing.T) {
	f := newSeatFixture(t, 5)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	if c, _ := f.change(p, ""); c != http.StatusOK {
		t.Fatal(c)
	}
	f.subscriptionUpdated() // Stripe's event for it
	n := f.quantityChanges(1)
	if n != 1 {
		t.Fatalf("quantity_changed recorded %d times, want 1", n)
	}
}

// People who join after a downgrade was checked, but before the licence
// is lowered, are counted under the lock: the downgrade is refused.
func TestSeats_DowngradeCountsAgainUnderTheLock(t *testing.T) {
	f := newSeatFixture(t, 5)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	inner := f.fake.serve(t)
	var joined int32
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		// The last look at the price before the licence is lowered.
		if r.URL.Path == "/v1/invoices/create_preview" && atomic.AddInt32(&joined, 1) == 1 {
			for i := range 2 {
				if _, _, err := f.s.AddSeatWithinLimit(f.ctx, &model.Seat{LicenseID: f.lic.ID, Email: fmt.Sprintf("late%d-%s", i, f.lic.Email), Role: "member"}, 5); err != nil {
					t.Errorf("join: %v", err)
				}
			}
		}
		inner(w, r)
	})
	if c, out := f.change(p, ""); c != http.StatusConflict || envCode(out) != "SEATS_IN_USE" {
		t.Fatalf("downgrade to 2 with 3 people = %d %v, want 409 SEATS_IN_USE", c, out)
	}
	if q := f.license().Quantity; q != 5 {
		t.Fatalf("quantity = %d, want 5", q)
	}
}

// A read of the subscription while a downgrade is with Stripe (the
// customer's own, through the pending banner, or Stripe's event) still
// shows the old seats: it does not put them back on the licence.
func TestSeats_ReadDuringADowngradeKeepsTheLowerSeats(t *testing.T) {
	for _, via := range []string{"pending", "event"} {
		t.Run(via, func(t *testing.T) {
			f := newSeatFixture(t, 6)
			p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
			f.fake.delay = 800 * time.Millisecond
			done := make(chan int, 1)
			go func() {
				c, _ := f.change(p, "")
				done <- c
			}()
			time.Sleep(200 * time.Millisecond)
			if via == "pending" {
				if c, out := f.post(f.h.PendingSubscriptionChange, f.lic.Email, fmt.Sprintf(`{"license_id":%q}`, f.lic.ID)); c != http.StatusOK {
					t.Fatalf("pending = %d %v", c, out)
				}
			} else {
				f.subscriptionUpdated()
			}
			if q := f.license().Quantity; q != 2 {
				t.Fatalf("quantity after a read while the downgrade is with Stripe = %d, want 2", q)
			}
			got := 0
			for i := range 4 {
				if _, _, err := f.s.AddSeatWithinLimit(f.ctx, &model.Seat{LicenseID: f.lic.ID, Email: fmt.Sprintf("r%d-%s", i, f.lic.Email), Role: "member"}, 6); err == nil {
					got++
				}
			}
			if c := <-done; c != http.StatusOK {
				t.Fatalf("downgrade = %d", c)
			}
			if got != 1 {
				t.Fatalf("%d invites got in during a downgrade to 2 seats, want 1", got)
			}
			f.subscriptionUpdated() // Stripe's event for the downgrade
			n := f.quantityChanges(1)
			if q := f.license().Quantity; q != 2 || n != 1 {
				t.Fatalf("after: quantity %d, quantity_changed recorded %d times; want 2 and once", q, n)
			}
		})
	}
}

// A downgrade Stripe refuses announces nothing: nothing changed.
func TestSeats_RefusedDowngradeAnnouncesNothing(t *testing.T) {
	f := newSeatFixture(t, 5)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	f.fake.updateErr = http.StatusBadRequest
	if c, _ := f.change(p, ""); c != http.StatusConflict {
		t.Fatal(c)
	}
	f.fake.updateErr = 0
	f.subscriptionUpdated()
	n := f.quantityChanges(0)
	if q := f.license().Quantity; q != 5 || n != 0 {
		t.Fatalf("after a refused downgrade: quantity %d, quantity_changed recorded %d times; want 5 and none", q, n)
	}
}

// A subscription set to end after this period still has a next invoice.
func TestSeats_DowngradeOfASubscriptionEndingLater(t *testing.T) {
	f := newSeatFixture(t, 5)
	f.fake.cancelAt = time.Now().Add(60 * 24 * time.Hour).Unix()
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":3}`, f.lic.ID))
	if p["downgrade"] != true || p["next_invoice_at"] == nil {
		t.Fatalf("preview = %v, want the next invoice shown", p)
	}
	f.fake.cancelAt = time.Now().Add(24 * time.Hour).Unix() // within this period
	p = f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":3}`, f.lic.ID))
	if p["next_invoice_at"] != nil {
		t.Fatalf("preview = %v, want no next invoice", p)
	}
}

// A preview token is accepted in one encoding only: a signature whose
// unused trailing bits were changed is an altered token.
func TestSeats_PreviewTokenOneEncoding(t *testing.T) {
	f := newSeatFixture(t, 2)
	tok := f.h.signChange(changeToken{License: f.lic.ID, Price: "p", Quantity: 3, ProrationDate: time.Now().Unix()})
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, tok[len(tok)-1])
	same := tok[:len(tok)-1] + string(alphabet[last^1]) // the bit no byte uses
	if _, ok := f.h.openChange(tok); !ok {
		t.Fatal("the token as signed was refused")
	}
	if _, ok := f.h.openChange(same); ok {
		t.Fatal("a token with its signature's unused bits changed was accepted")
	}
}

// Stripe made a downgrade but answered 503: asked again with the same
// key, it answers what it made, and the downgrade is applied and
// announced, not taken back.
func TestSeats_DowngradeMadeButUnansweredIsAskedAgain(t *testing.T) {
	f := newSeatFixture(t, 5)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	inner := f.fake.serve(t)
	var posts int32
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/subscriptions/"+f.fake.id && atomic.AddInt32(&posts, 1) == 1 {
			inner(httptest.NewRecorder(), r) // made
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"type":"api_error","message":"try later"}}`))
			return
		}
		inner(w, r)
	})
	if c, out := f.change(p, ""); c != http.StatusOK || envData(out)["status"] != "applied" {
		t.Fatalf("change = %d %v, want applied", c, out)
	}
	n := f.quantityChanges(1)
	if q := f.license().Quantity; q != 2 || n != 1 {
		t.Fatalf("quantity %d, announced %d times; want 2, once", q, n)
	}
}

// An edit in the Stripe dashboard read while a downgrade's terms were
// held is not lost: the licence ends on what Stripe bills.
func TestSeats_EditDuringARefusedDowngradeIsKept(t *testing.T) {
	f := newSeatFixture(t, 6)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	inner := f.fake.serve(t)
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/subscriptions/"+f.fake.id {
			// Meanwhile the seller sets 3 seats, and its event is read.
			f.fake.set(f.team.StripePriceID, 3)
			f.subscriptionUpdated()
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"no"}}`))
			return
		}
		inner(w, r)
	})
	if c, out := f.change(p, ""); c != http.StatusConflict || envCode(out) != "CHANGE_REFUSED" {
		t.Fatalf("change = %d %v, want CHANGE_REFUSED", c, out)
	}
	if q := f.license().Quantity; q != 3 {
		t.Fatalf("quantity = %d, want the 3 Stripe bills", q)
	}
}

// A downgrade whose outcome was never known leaves the licence lower; the
// customer's next look at their current seats brings it into line.
func TestSeats_UnknownDowngradeIsBroughtIntoLine(t *testing.T) {
	f := newSeatFixture(t, 5)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	inner := f.fake.serve(t)
	// Stripe goes down as the change is asked for: nothing made, nothing
	// read after.
	var down, asked atomic.Bool
	down.Store(true)
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/subscriptions/"+f.fake.id {
			asked.Store(true)
		}
		if down.Load() && asked.Load() && r.URL.Path == "/v1/subscriptions/"+f.fake.id {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"type":"api_error","message":"down"}}`))
			return
		}
		inner(w, r)
	})
	if c, _ := f.change(p, ""); c != http.StatusAccepted {
		t.Fatalf("change = %d, want 202", c)
	}
	if q := f.license().Quantity; q != 2 {
		t.Fatalf("quantity while unknown = %d, want the lower 2 kept", q)
	}
	down.Store(false)
	f.endHold() // Stripe can no longer be making it
	if c, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"quantity":5}`, f.lic.ID)); c != http.StatusConflict || envCode(out) != "NO_CHANGE" {
		t.Fatalf("preview of the 5 seats it has = %d %v, want NO_CHANGE", c, out)
	}
	if q := f.license().Quantity; q != 5 {
		t.Fatalf("quantity after that look = %d, want the 5 Stripe bills", q)
	}
}

// A change Stripe gave no answer for is asked again with the same key,
// and goes through when Stripe answers then.
func TestSeats_UnansweredChangeIsAskedAgain(t *testing.T) {
	f := newSeatFixture(t, 5)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	inner := f.fake.serve(t)
	var posts int32
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		// Every attempt stripe-go itself makes fails; ours, later, does not.
		if r.Method == http.MethodPost && r.URL.Path == "/v1/subscriptions/"+f.fake.id && atomic.AddInt32(&posts, 1) <= 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"type":"api_error","message":"try later"}}`))
			return
		}
		inner(w, r)
	})
	if c, out := f.change(p, ""); c != http.StatusOK || envData(out)["status"] != "applied" {
		t.Fatalf("change = %d %v, want applied once asked again", c, out)
	}
	if q := f.license().Quantity; q != 2 {
		t.Fatalf("quantity = %d, want 2", q)
	}
}

// A downgrade already made, sent again, answers applied, whatever the
// payment method: it is a downgrade no longer, and nothing is checked.
func TestSeats_DowngradeSentAgainOnBankDebit(t *testing.T) {
	f := newSeatFixture(t, 5)
	f.fake.pmType = "sepa_debit"
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	if c, _ := f.change(p, ""); c != http.StatusOK {
		t.Fatal(c)
	}
	if c, out := f.change(p, ""); c != http.StatusOK || envData(out)["status"] != "applied" {
		t.Fatalf("the same downgrade again = %d %v, want applied", c, out)
	}
}

// A downgrade whose outcome is unknown still applies what the read after
// it shows, its seats aside: a renewal is not left for the next event.
func TestSeats_UnknownDowngradeAppliesTheRead(t *testing.T) {
	f := newSeatFixture(t, 5)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	renewed := time.Now().Add(45 * 24 * time.Hour).Unix()
	f.fake.mu.Lock()
	f.fake.periodEnd = renewed
	f.fake.mu.Unlock()
	f.fake.updateErr = http.StatusServiceUnavailable
	if c, _ := f.change(p, ""); c != http.StatusAccepted {
		t.Fatalf("change = %d, want 202", c)
	}
	l := f.license()
	if l.ValidUntil == nil || l.ValidUntil.Unix() != renewed || l.Quantity != 2 {
		t.Fatalf("licence: valid until %v, quantity %d; want the renewed period and the lower 2", l.ValidUntil, l.Quantity)
	}
}

// quantityChanges counts the licence's quantity_changed audit entries
// once want of them are there. They are written in the background
// (Store.Audit), so it waits for them, up to five seconds, and then a
// moment longer for any extra one to arrive: a fixed sleep was outlasted
// on a loaded machine, failing a test whose behaviour was right. With
// want 0 there is nothing to wait for but that moment.
func (f *seatFixture) quantityChanges(want int) int {
	f.t.Helper()
	read := func() int {
		var n int
		if err := f.s.DB.NewRaw("SELECT COUNT(*) FROM audit_logs WHERE entity_id = ? AND action = 'quantity_changed'",
			f.lic.ID).Scan(f.ctx, &n); err != nil {
			f.t.Fatal(err)
		}
		return n
	}
	for deadline := time.Now().Add(5 * time.Second); read() < want && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	return read()
}

// A downgrade that ends unanswered is announced as the licence moves:
// once when it takes the lower seats, and once more, the other way, if
// Stripe turns out not to have made it.
func TestSeats_UnknownDowngradeAnnouncedAsTheLicenceMoves(t *testing.T) {
	f := newSeatFixture(t, 5)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	f.fake.updateErr = http.StatusServiceUnavailable
	if c, _ := f.change(p, ""); c != http.StatusAccepted {
		t.Fatalf("change = %d, want 202", c)
	}
	if n := f.quantityChanges(1); n != 1 {
		t.Fatalf("announced %d times on the lower seats, want once", n)
	}
	f.fake.updateErr = 0
	f.subscriptionUpdated() // Stripe still bills 5, but may yet make it: held
	if q, n := f.license().Quantity, f.quantityChanges(1); q != 2 || n != 1 {
		t.Fatalf("while held: quantity %d, announced %d times; want 2 and once", q, n)
	}
	f.endHold()
	f.subscriptionUpdated() // the hold is over, and Stripe still bills 5: not made
	if q, n := f.license().Quantity, f.quantityChanges(2); q != 5 || n != 2 {
		t.Fatalf("after Stripe showed it not made: quantity %d, announced %d times; want 5 and twice", q, n)
	}
}

// A change that lowers one limit while raising the other is refused: the
// licence would take the higher one before Stripe made the change.
func TestSeats_MixedDowngradeIsRefused(t *testing.T) {
	f := newSeatFixture(t, 3) // 3 seats × 2 devices
	wide := &model.Plan{ProductID: f.prod.ID, Name: "Wide", Slug: "wide-" + f.suffix, LicenseType: "subscription",
		LicenseModel: "standard", Active: true, StripePriceID: "price_wide_" + f.suffix,
		PerSeat: true, MinQuantity: 1, MaxQuantity: 20, MaxActivations: 1, MaxSeats: 1}
	if err := f.s.CreatePlan(f.ctx, wide); err != nil {
		t.Fatal(err)
	}
	f.fake.unit[wide.StripePriceID] = 100
	body := fmt.Sprintf(`{"license_id":%q,"price_id":%q,"quantity":5}`, f.lic.ID, wide.StripePriceID) // 5 seats, 5 devices
	if c, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, body); c != http.StatusConflict || envCode(out) != "DOWNGRADE_NOT_SUPPORTED" {
		t.Fatalf("more seats, fewer devices = %d %v, want DOWNGRADE_NOT_SUPPORTED", c, out)
	}
	if q := f.license().Quantity; q != 3 {
		t.Fatalf("quantity = %d, want 3", q)
	}
}

// The same downgrade sent again after it ended unanswered is announced
// once: the licence moved once.
func TestSeats_DowngradeSentAgainAnnouncedOnce(t *testing.T) {
	f := newSeatFixture(t, 5)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	f.fake.updateErr = http.StatusServiceUnavailable
	if c, _ := f.change(p, ""); c != http.StatusAccepted {
		t.Fatalf("change = %d, want 202", c)
	}
	f.fake.updateErr = 0
	if c, out := f.change(p, ""); c != http.StatusOK || envData(out)["status"] != "applied" {
		t.Fatalf("again = %d %v, want applied", c, out)
	}
	if n := f.quantityChanges(1); n != 1 {
		t.Fatalf("announced %d times, want once", n)
	}
}

// A quantity a seller set in Stripe on a plan not sold per seat moves no
// limit, and is not announced.
func TestSeats_QuantityOnAPlanNotPerSeatIsNotAnnounced(t *testing.T) {
	f := newSeatFixture(t, 1)
	f.fake.set(f.solo.StripePriceID, 1)
	f.subscriptionUpdated()
	before := f.quantityChanges(0)
	f.fake.set(f.solo.StripePriceID, 3)
	f.subscriptionUpdated()
	n := f.quantityChanges(0)
	if q := f.license().Quantity; q != 3 || n != before {
		t.Fatalf("quantity %d, quantity_changed %d more; want 3 and none", q, n-before)
	}
}

// A read that saw the subscription before a downgrade was made, landing
// just after its terms went free, does not put the old seats back.
func TestSeats_LateReadAfterADowngradeIsOutOfDate(t *testing.T) {
	f := newSeatFixture(t, 6)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	inner := f.fake.serve(t)
	var gets, armed int32
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&armed) == 1 && r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions/"+f.lic.StripeSubscriptionID {
			switch atomic.AddInt32(&gets, 1) {
			case 3: // the customer's own read: answered now (old seats), delivered late
				rec := httptest.NewRecorder()
				inner(rec, r)
				time.Sleep(700 * time.Millisecond)
				for k, v := range rec.Header() {
					w.Header()[k] = v
				}
				w.WriteHeader(rec.Code)
				_, _ = w.Write(rec.Body.Bytes())
				return
			case 4: // the change's last read: slow
				time.Sleep(900 * time.Millisecond)
			}
		}
		inner(w, r)
	})
	f.fake.delay = 500 * time.Millisecond
	atomic.StoreInt32(&armed, 1)
	done := make(chan int, 1)
	go func() {
		c, _ := f.change(p, "")
		done <- c
	}()
	time.Sleep(150 * time.Millisecond)
	f.post(f.h.PendingSubscriptionChange, f.lic.Email, fmt.Sprintf(`{"license_id":%q}`, f.lic.ID))
	got := 0
	for i := range 4 {
		if _, _, err := f.s.AddSeatWithinLimit(f.ctx, &model.Seat{LicenseID: f.lic.ID, Email: fmt.Sprintf("x%d-%s", i, f.lic.Email), Role: "member"}, 6); err == nil {
			got++
		}
	}
	if c := <-done; c != http.StatusOK {
		t.Fatalf("downgrade = %d", c)
	}
	if got > 1 {
		t.Fatalf("a downgrade to 2 seats let %d invites in", got)
	}
}

// A downgrade Stripe made but whose answer was lost, and whose read back
// fails, ends in processing; a read that saw the old seats before, landing
// after, does not put them back.
func TestSeats_LateReadAfterAnUnansweredDowngrade(t *testing.T) {
	for _, every := range []bool{false, true} { // the retry replays it, or every answer is lost
		t.Run(fmt.Sprintf("every answer lost %v", every), func(t *testing.T) { lateReadAfterAnUnansweredDowngrade(t, every) })
	}
}

func lateReadAfterAnUnansweredDowngrade(t *testing.T, everyAnswerLost bool) {
	f := newSeatFixture(t, 6)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	inner := f.fake.serve(t)
	var posts, gets, armed int32
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&armed) == 1 && r.URL.Path == "/v1/subscriptions/"+f.lic.StripeSubscriptionID {
			if r.Method == http.MethodPost {
				if n := atomic.AddInt32(&posts, 1); n == 1 || everyAnswerLost {
					if n == 1 {
						time.Sleep(400 * time.Millisecond)
						inner(httptest.NewRecorder(), r) // made, but the answer is lost
					}
					w.WriteHeader(http.StatusBadGateway)
					_, _ = w.Write([]byte(`{"error":{"type":"api_error","message":"lost"}}`))
					return
				}
			}
			if r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "latest_invoice") {
				if atomic.AddInt32(&gets, 1) == 1 {
					rec := httptest.NewRecorder()
					inner(rec, r) // the customer's read: old seats, delivered late
					time.Sleep(2500 * time.Millisecond)
					for k, v := range rec.Header() {
						w.Header()[k] = v
					}
					w.WriteHeader(rec.Code)
					_, _ = w.Write(rec.Body.Bytes())
					return
				}
				w.WriteHeader(http.StatusBadRequest) // the change's own reads fail
				_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"down"}}`))
				return
			}
		}
		inner(w, r)
	})
	atomic.StoreInt32(&armed, 1)
	done := make(chan int, 1)
	go func() {
		c, _ := f.change(p, "")
		done <- c
	}()
	time.Sleep(150 * time.Millisecond)
	f.post(f.h.PendingSubscriptionChange, f.lic.Email, fmt.Sprintf(`{"license_id":%q}`, f.lic.ID))
	if c := <-done; c != http.StatusAccepted {
		t.Fatalf("change = %d, want 202", c)
	}
	if q := f.license().Quantity; q != 2 {
		t.Fatalf("licence holds %d seats, Stripe bills 2", q)
	}
}

// A downgrade whose outcome is unknown keeps its lower seats against
// reads that still show the old ones, after its request is over and
// across a restart, until Stripe shows the change or the hold runs out.
func TestSeats_UnknownDowngradeHoldOutlivesTheRequest(t *testing.T) {
	f := newSeatFixture(t, 6)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	f.fake.updateErr = http.StatusServiceUnavailable
	if c, _ := f.change(p, ""); c != http.StatusAccepted {
		t.Fatalf("change = %d, want 202", c)
	}
	f.fake.updateErr = 0
	// The request is over; a read that still shows 6 seats, from the
	// customer or Stripe, keeps the 2.
	f.post(f.h.PendingSubscriptionChange, f.lic.Email, fmt.Sprintf(`{"license_id":%q}`, f.lic.ID))
	f.subscriptionUpdated()
	if q := f.license().Quantity; q != 2 {
		t.Fatalf("quantity after reads of the old seats = %d, want the lower 2 held", q)
	}
	got := 0
	for i := range 4 {
		if _, _, err := f.s.AddSeatWithinLimit(f.ctx, &model.Seat{LicenseID: f.lic.ID, Email: fmt.Sprintf("h%d-%s", i, f.lic.Email), Role: "member"}, 6); err == nil {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("%d invites got in while held at 2 seats, want 1", got)
	}
	// Stripe makes it after all: the read that shows it ends the hold.
	f.fake.set(f.team.StripePriceID, 2)
	f.subscriptionUpdated()
	var held *time.Time
	if err := f.s.DB.NewRaw("SELECT terms_held_until FROM licenses WHERE id = ?", f.lic.ID).Scan(f.ctx, &held); err != nil || held != nil {
		t.Fatalf("hold after a read that shows the change = %v (%v), want none", held, err)
	}
}

// A webhook whose seats a downgrade holds writes the rest, and asks to
// be sent again.
func TestSeats_HeldWebhookIsRetried(t *testing.T) {
	f := newSeatFixture(t, 6)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	f.fake.updateErr = http.StatusServiceUnavailable
	if c, _ := f.change(p, ""); c != http.StatusAccepted {
		t.Fatalf("change = %d, want 202", c)
	}
	f.fake.updateErr = 0
	err := f.h.onSubscriptionUpdated(f.ctx, fmt.Appendf(nil, `{"id":%q}`, f.lic.StripeSubscriptionID))
	if !errors.Is(err, store.ErrTermsHeld) {
		t.Fatalf("webhook while held = %v, want ErrTermsHeld so Stripe sends it again", err)
	}
}

// A downgrade Stripe made ends its hold at once: an edit in the Stripe
// dashboard right after is not kept out for the hold's whole time.
func TestSeats_MadeDowngradeEndsItsHold(t *testing.T) {
	f := newSeatFixture(t, 6)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	inner := f.fake.serve(t)
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		inner(w, r)
		if r.Method == http.MethodPost && r.URL.Path == "/v1/subscriptions/"+f.fake.id {
			f.fake.set(f.team.StripePriceID, 3) // the seller, a moment later
		}
	})
	if c, out := f.change(p, ""); c != http.StatusOK {
		t.Fatalf("change = %d %v", c, out)
	}
	if q := f.license().Quantity; q != 3 {
		t.Fatalf("quantity = %d, want the 3 Stripe bills now", q)
	}
}

// While a downgrade's outcome is unknown, another change waits: checked
// against the subscription as Stripe still shows it, it would overwrite
// the hold. The same change, sent again, goes on.
func TestSeats_OtherChangeWaitsForAnUnknownDowngrade(t *testing.T) {
	f := newSeatFixture(t, 6)
	first := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	f.fake.updateErr = http.StatusServiceUnavailable
	if c, _ := f.change(first, ""); c != http.StatusAccepted {
		t.Fatalf("first = %d, want 202", c)
	}
	f.fake.updateErr = 0
	if c, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"quantity":4}`, f.lic.ID)); c != http.StatusConflict || envCode(out) != "CHANGE_IN_PROGRESS" {
		t.Fatalf("another change while the first is unknown = %d %v, want 409 CHANGE_IN_PROGRESS", c, out)
	}
	if c, out := f.change(first, ""); c != http.StatusOK || envData(out)["status"] != "applied" {
		t.Fatalf("the first sent again = %d %v, want applied", c, out)
	}
	if q := f.license().Quantity; q != 2 {
		t.Fatalf("quantity = %d, want 2", q)
	}
}

// A downgrade whose outcome was never known, and that Stripe never made,
// gives the licence its terms back once the hold runs out, without an
// event or a look from the customer.
func TestSeats_LapsedHoldIsSettled(t *testing.T) {
	f := newSeatFixture(t, 6)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	f.fake.updateErr = http.StatusServiceUnavailable
	if c, _ := f.change(p, ""); c != http.StatusAccepted {
		t.Fatalf("change = %d, want 202", c)
	}
	f.fake.updateErr = 0
	// The sweep is for every licence: other tests' holds are not this
	// fake's to answer.
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET terms_held_until = NULL WHERE id <> ?", f.lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.h.SyncLapsedTermHolds(f.ctx)
	if q := f.license().Quantity; q != 2 {
		t.Fatalf("quantity while held = %d, want 2", q)
	}
	f.endHold()
	f.h.SyncLapsedTermHolds(f.ctx)
	var held *time.Time
	if err := f.s.DB.NewRaw("SELECT terms_held_until FROM licenses WHERE id = ?", f.lic.ID).Scan(f.ctx, &held); err != nil {
		t.Fatal(err)
	}
	if q := f.license().Quantity; q != 6 || held != nil {
		t.Fatalf("after the sweep: quantity %d, hold %v; want the 6 Stripe bills and none", q, held)
	}
}

// A seat write that fails (the database, not a stale or refused read)
// fails the event, so Stripe sends it again.
func TestSeats_FailedSeatWriteIsRetried(t *testing.T) {
	f := newSeatFixture(t, 5)
	fn := "fail_seats_" + f.suffix
	if _, err := f.s.DB.ExecContext(f.ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'database unavailable'; END $$`, fn)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.ExecContext(f.ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON licenses FOR EACH ROW
		WHEN (NEW.id = '%s' AND NEW.quantity IS DISTINCT FROM OLD.quantity) EXECUTE FUNCTION %s()`, fn, f.lic.ID, fn)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.s.DB.ExecContext(context.Background(), fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON licenses", fn))
		_, _ = f.s.DB.ExecContext(context.Background(), fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", fn))
	})
	f.fake.set(f.team.StripePriceID, 3)
	err := f.h.onSubscriptionUpdated(f.ctx, fmt.Appendf(nil, `{"id":%q}`, f.lic.StripeSubscriptionID))
	if err == nil {
		t.Fatal("a failed seat write was taken for done; want an error so Stripe sends the event again")
	}
}

// holdState reads a licence's hold and the time of its next check.
func (f *seatFixture) holdState() (held, checkAfter *time.Time) {
	f.t.Helper()
	var row struct {
		Held  *time.Time `bun:"terms_held_until"`
		Check *time.Time `bun:"terms_check_after"`
	}
	if err := f.s.DB.NewRaw("SELECT terms_held_until, terms_check_after FROM licenses WHERE id = ?", f.lic.ID).Scan(f.ctx, &row); err != nil {
		f.t.Fatal(err)
	}
	return row.Held, row.Check
}

// lapsedHold leaves the licence with a downgrade hold that ran out
// unsettled, and no other licence's in the way of the sweep.
func (f *seatFixture) lapsedHold() {
	f.t.Helper()
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	f.fake.updateErr = http.StatusServiceUnavailable
	if c, _ := f.change(p, ""); c != http.StatusAccepted {
		f.t.Fatalf("change = %d, want 202", c)
	}
	f.fake.updateErr = 0
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET terms_held_until = NULL, terms_check_after = NULL WHERE id <> ?", f.lic.ID).Exec(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	f.endHold()
}

// A lapsed hold on a subscription that has ended is settled by the read,
// though it writes no seats: it is not picked again and again.
func TestSeats_LapsedHoldOnAnEndedSubscriptionIsSettled(t *testing.T) {
	f := newSeatFixture(t, 6)
	f.lapsedHold()
	f.fake.mu.Lock()
	f.fake.status = "canceled"
	f.fake.mu.Unlock()
	f.h.SyncLapsedTermHolds(f.ctx)
	if held, _ := f.holdState(); held != nil {
		t.Fatalf("hold after the sweep read an ended subscription = %v, want none", held)
	}
}

// A lapsed hold whose subscription this API key cannot see is kept, and
// checked again later rather than taken for settled.
func TestSeats_LapsedHoldNotVisibleIsKept(t *testing.T) {
	f := newSeatFixture(t, 6)
	f.lapsedHold()
	inner := f.fake.serve(t)
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions/"+f.fake.id {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"resource_missing","message":"No such subscription"}}`))
			return
		}
		inner(w, r)
	})
	f.h.SyncLapsedTermHolds(f.ctx)
	held, check := f.holdState()
	if held == nil || check == nil || time.Until(*check) < 50*time.Minute {
		t.Fatalf("after a lookup that could not see it: hold %v, next check %v; want both, an hour away", held, check)
	}
	if holds, err := f.s.FindLapsedTermHolds(f.ctx, 50); err != nil || len(holds) != 0 {
		t.Fatalf("lapsed holds due now = %v (%v), want none until its next check", holds, err)
	}
}

// A lapsed hold whose check fails waits behind the others, which are
// settled meanwhile.
func TestSeats_FailedHoldCheckDoesNotBlockOthers(t *testing.T) {
	f := newSeatFixture(t, 6)
	f.lapsedHold()
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET terms_check_after = now() - interval '1 hour' WHERE id = ?", f.lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	failing := newSeatFixture(t, 6) // its own fake Stripe now answers for this test
	failing.lapsedHold()
	// The first fixture's licence was cleared by the second's setup; put
	// it back as lapsed, behind the failing one's earlier check.
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET terms_held_until = now() - interval '1 second', terms_check_after = NULL WHERE id = ?", f.lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET terms_check_after = now() - interval '2 hours' WHERE id = ?", failing.lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	failing.fake.updateErr = 0
	inner := failing.fake.serve(t)
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions/"+failing.fake.id {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"type":"api_error","message":"down"}}`))
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions/"+f.fake.id {
			f.fake.serve(t)(w, r)
			return
		}
		inner(w, r)
	})
	f.h.SyncLapsedTermHolds(f.ctx)
	if held, _ := f.holdState(); held != nil {
		t.Fatalf("the other licence's hold = %v, want it settled despite the failing one", held)
	}
	if held, check := failing.holdState(); held == nil || check == nil || time.Until(*check) < 5*time.Minute {
		t.Fatalf("the failing licence: hold %v, next check %v; want it kept and put off", held, check)
	}
}

// The sweep leaves alone a licence whose change lock is taken (a change
// under way), and settles only the hold it found, not one a downgrade
// placed since.
func TestSeats_SweepLeavesANewDowngradeAlone(t *testing.T) {
	f := newSeatFixture(t, 6)
	f.lapsedHold()
	lock, err := f.s.LockLicenseChange(f.ctx, f.lic.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.h.SyncLapsedTermHolds(f.ctx)
	lock.Unlock()
	if held, _ := f.holdState(); held == nil {
		t.Fatal("the sweep settled a hold while a change was under way")
	}
	holds, err := f.s.FindLapsedTermHolds(f.ctx, 50)
	if err != nil || len(holds) != 1 {
		t.Fatalf("lapsed holds = %v (%v), want this one", holds, err)
	}
	// A new downgrade holds the licence before the sweep gets to settle
	// what it found.
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET terms_held_until = now() + interval '10 minutes' WHERE id = ?", f.lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SettleLapsedHold(f.ctx, holds[0]); err != nil {
		t.Fatal(err)
	}
	if held, _ := f.holdState(); held == nil || time.Until(*held) < 5*time.Minute {
		t.Fatalf("hold after settling the old one = %v, want the new downgrade's kept", held)
	}
}

// Keeping the seats the subscription has, to drop a change waiting for
// payment, is not refused when the plan's range has since moved past
// them.
func TestSeats_KeepOutsideANarrowedRange(t *testing.T) {
	f := newSeatFixture(t, 2)
	f.fake.failPayment = true
	up := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":6}`, f.lic.ID))
	if c, out := f.change(up, ""); c != http.StatusOK || envData(out)["status"] != "pending_payment" {
		t.Fatalf("held increase = %d %v", c, out)
	}
	if _, err := f.s.DB.NewRaw("UPDATE plans SET min_quantity = 3 WHERE id = ?", f.team.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	keep := f.preview(fmt.Sprintf(`{"license_id":%q}`, f.lic.ID))
	if c, out := f.change(keep, ""); c != http.StatusOK || envData(out)["status"] != "applied" {
		t.Fatalf("keeping 2 seats under a minimum of 3 = %d %v, want applied", c, out)
	}
	if l := f.license(); l.Quantity != 2 || l.StripePendingUpdateExpiresAt != nil {
		t.Fatalf("licence: quantity %d pending %v, want 2 and nothing waiting", l.Quantity, l.StripePendingUpdateExpiresAt)
	}
	// A new number below the minimum is still refused.
	if c, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"quantity":1}`, f.lic.ID)); c != http.StatusConflict || envCode(out) != "QUANTITY_OUT_OF_RANGE" {
		t.Fatalf("1 seat under a minimum of 3 = %d %v, want QUANTITY_OUT_OF_RANGE", c, out)
	}
}

// On a price no plan sells any more, a downgrade to another plan is
// refused: a read would not show which plan the subscription ended on.
// Fewer seats on the same plan still go ahead.
func TestSeats_RetiredPriceDowngrades(t *testing.T) {
	f := newSeatFixture(t, 3)
	lite := &model.Plan{ProductID: f.prod.ID, Name: "Lite", Slug: "lite-" + f.suffix, LicenseType: "subscription",
		LicenseModel: "standard", Active: true, StripePriceID: "price_lite_" + f.suffix,
		PerSeat: true, MinQuantity: 1, MaxQuantity: 20, MaxActivations: 1, MaxSeats: 1}
	if err := f.s.CreatePlan(f.ctx, lite); err != nil {
		t.Fatal(err)
	}
	f.fake.unit[lite.StripePriceID] = 500
	// The team plan moves to a new price; the subscription keeps the old.
	if _, err := f.s.DB.NewRaw("UPDATE plans SET stripe_price_id = ? WHERE id = ?", "price_team_new_"+f.suffix, f.team.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"license_id":%q,"price_id":%q,"quantity":3}`, f.lic.ID, lite.StripePriceID)
	if c, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, body); c != http.StatusConflict || envCode(out) != "DOWNGRADE_NOT_SUPPORTED" {
		t.Fatalf("to another plan from a retired price = %d %v, want DOWNGRADE_NOT_SUPPORTED", c, out)
	}
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	if c, out := f.change(p, ""); c != http.StatusOK || envData(out)["status"] != "applied" {
		t.Fatalf("fewer seats on the retired price = %d %v, want applied", c, out)
	}
	if l := f.license(); l.PlanID != f.team.ID || l.Quantity != 2 {
		t.Fatalf("licence: plan %s quantity %d, want team with 2", l.PlanID, l.Quantity)
	}
}

// A preview while a downgrade's outcome is unknown is judged on the
// licence's held terms: the same change can be previewed again; going
// back to the old seats waits.
func TestSeats_PreviewDuringAHold(t *testing.T) {
	f := newSeatFixture(t, 6)
	first := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	f.fake.updateErr = http.StatusServiceUnavailable
	if c, _ := f.change(first, ""); c != http.StatusAccepted {
		t.Fatalf("first = %d, want 202", c)
	}
	f.fake.updateErr = 0
	if c, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID)); c != http.StatusOK {
		t.Fatalf("the same change previewed again = %d %v, want a preview", c, out)
	}
	if c, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"quantity":6}`, f.lic.ID)); c != http.StatusConflict || envCode(out) != "CHANGE_IN_PROGRESS" {
		t.Fatalf("back to 6 while held = %d %v, want CHANGE_IN_PROGRESS", c, out)
	}
}

// While a checkout for upgrading to a plan sold per seat is open, nobody
// new joins: its seats were counted from the people there when it opened.
// An invite sent again is not someone joining; a checkout too old to be
// paid holds nothing up.
func TestUpgrade_NoInvitesWhileTheCheckoutIsOpen(t *testing.T) {
	f := newUpgradeFixture(t, "upfreeze")
	if _, err := f.s.DB.NewRaw("UPDATE plans SET per_seat = true, min_quantity = 1, max_quantity = 20 WHERE id = ?", f.paid.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.NewRaw("UPDATE plans SET max_seats = 10 WHERE id = ?", f.free.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	invite := func(email string) error {
		_, hash := store.GenerateSeatInviteToken()
		_, _, err := f.s.AddSeatWithinLimit(f.ctx, &model.Seat{LicenseID: f.lic.ID, Email: email + "-" + f.lic.Email, Role: "member", InviteTokenHash: hash}, 10)
		return err
	}
	if err := invite("before"); err != nil {
		t.Fatalf("invite before any checkout: %v", err)
	}
	u := &model.LicenseUpgrade{LicenseID: f.lic.ID, ToPlanID: f.paid.ID, StripeCheckoutSessionID: "cs_freeze_" + f.tag}
	if err := store.CreateLicenseUpgradeIn(f.ctx, f.s.DB, u); err != nil {
		t.Fatal(err)
	}
	if err := invite("during"); !errors.Is(err, store.ErrUpgradeCheckoutOpen) {
		t.Fatalf("invite while the checkout is open = %v, want ErrUpgradeCheckoutOpen", err)
	}
	if err := invite("before"); err != nil {
		t.Fatalf("the same invite sent again while the checkout is open: %v", err)
	}
	if _, err := f.s.DB.NewRaw("UPDATE license_upgrades SET created_at = now() - interval '25 hours' WHERE id = ?", u.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := invite("after"); err != nil {
		t.Fatalf("invite once the checkout can no longer be paid: %v", err)
	}
}

// A paid upgrade applies as paid even if the licence holds more people
// than the seats bought (an operator added one meanwhile): everyone
// stays, and nobody new joins until seats are added or freed.
func TestUpgrade_PerSeatOverItsSeatsAppliesAsPaid(t *testing.T) {
	f := newUpgradeFixture(t, "upover")
	if _, err := f.s.DB.NewRaw("UPDATE plans SET per_seat = true, min_quantity = 1, max_quantity = 20 WHERE id = ?", f.paid.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := f.s.CreateSeat(f.ctx, &model.Seat{LicenseID: f.lic.ID, Email: fmt.Sprintf("op%d-%s", i, f.lic.Email), Role: "member"}); err != nil {
			t.Fatal(err)
		}
	}
	subID := "sub_over_" + f.tag
	sub := newFakeSubscription(subID, f.paid.StripePriceID, 1, nil)
	stubStripe(t, sub.serve(t))
	f.applySub(t, "cs_over_"+f.tag, subID)
	if l := f.reload(t); l.PlanID != f.paid.ID || l.Quantity != 1 {
		t.Fatalf("licence: plan %s quantity %d, want the paid plan with the 1 seat bought", l.PlanID, l.Quantity)
	}
	if n, _ := f.s.CountActiveSeats(f.ctx, f.lic.ID); n != 2 {
		t.Fatalf("members = %d, want both kept", n)
	}
	if _, _, err := f.s.AddSeatWithinLimit(f.ctx, &model.Seat{LicenseID: f.lic.ID, Email: "new-" + f.lic.Email, Role: "member"}, 1); !errors.Is(err, store.ErrSeatLimitReached) {
		t.Fatalf("invite over the seats bought = %v, want ErrSeatLimitReached", err)
	}
}

// A licence an operator suspended takes the plan Stripe bills on its
// subscription row too.
func TestSeats_SuspendedLicenceSubscriptionRowFollowsThePlan(t *testing.T) {
	f := newSeatFixture(t, 3)
	other := &model.Plan{ProductID: f.prod.ID, Name: "Other", Slug: "susp-" + f.suffix, LicenseType: "subscription",
		LicenseModel: "standard", Active: true, StripePriceID: "price_susp_" + f.suffix,
		PerSeat: true, MinQuantity: 1, MaxQuantity: 20, MaxActivations: 3, MaxSeats: 1}
	if err := f.s.CreatePlan(f.ctx, other); err != nil {
		t.Fatal(err)
	}
	f.fake.unit[other.StripePriceID] = 1500
	if err := f.s.SuspendLicense(f.ctx, f.lic.ID); err != nil {
		t.Fatal(err)
	}
	f.fake.set(other.StripePriceID, 3)
	f.subscriptionUpdated()
	var planID string
	if err := f.s.DB.NewRaw("SELECT plan_id FROM subscriptions WHERE license_id = ?", f.lic.ID).Scan(f.ctx, &planID); err != nil {
		t.Fatal(err)
	}
	if l := f.license(); l.PlanID != other.ID || planID != other.ID {
		t.Fatalf("licence plan %s, subscription row plan %s; want both on the plan Stripe bills", l.PlanID, planID)
	}
}

// A downgrade sent again after its outcome was unknown, and refused by
// Stripe this time, leaves the licence held, not settled on the lowered
// seats: the sweep gives it back what Stripe bills even if no read
// follows.
func TestSeats_RefusedRetryOfAnUnknownDowngradeKeepsTheHold(t *testing.T) {
	f := newSeatFixture(t, 6)
	p := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":2}`, f.lic.ID))
	f.fake.updateErr = http.StatusServiceUnavailable
	if c, _ := f.change(p, ""); c != http.StatusAccepted {
		t.Fatalf("first = %d, want 202", c)
	}
	f.fake.updateErr = http.StatusBadRequest
	if c, out := f.change(p, ""); c != http.StatusConflict || envCode(out) != "CHANGE_REFUSED" {
		t.Fatalf("retry = %d %v, want CHANGE_REFUSED", c, out)
	}
	if held, _ := f.holdState(); held == nil {
		t.Fatal("hold gone after a refused retry; nothing would bring back the seats Stripe bills")
	}
	f.fake.updateErr = 0
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET terms_held_until = NULL, terms_check_after = NULL WHERE id <> ?", f.lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.endHold()
	f.h.SyncLapsedTermHolds(f.ctx)
	if q := f.license().Quantity; q != 6 {
		t.Fatalf("quantity after the sweep = %d, want the 6 Stripe bills", q)
	}
}

// An invite made while an upgrade checkout is being opened waits for it
// (the people are counted under the same lock), and is then held up by
// it: nobody joins between the count and the checkout.
func TestUpgrade_InviteWaitsForTheCheckoutBeingOpened(t *testing.T) {
	f := newUpgradeFixture(t, "upwait")
	if _, err := f.s.DB.NewRaw("UPDATE plans SET per_seat = true, min_quantity = 1, max_quantity = 20 WHERE id = ?", f.paid.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	opened := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- f.s.WithXactLock(f.ctx, store.UpgradeLockKey(f.lic.ID), func(ctx context.Context, tx bun.Tx) error {
			close(opened)
			<-release
			return store.CreateLicenseUpgradeIn(ctx, tx, &model.LicenseUpgrade{LicenseID: f.lic.ID, ToPlanID: f.paid.ID, StripeCheckoutSessionID: "cs_wait_" + f.tag})
		})
	}()
	<-opened
	invited := make(chan error, 1)
	go func() {
		_, hash := store.GenerateSeatInviteToken()
		_, _, err := f.s.AddSeatWithinLimit(f.ctx, &model.Seat{LicenseID: f.lic.ID, Email: "late-" + f.lic.Email, Role: "member", InviteTokenHash: hash}, 10)
		invited <- err
	}()
	select {
	case err := <-invited:
		t.Fatalf("invite went ahead while the checkout was being opened: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-invited; !errors.Is(err, store.ErrUpgradeCheckoutOpen) {
		t.Fatalf("invite once the checkout is open = %v, want ErrUpgradeCheckoutOpen", err)
	}
}

// Dropping a waiting change charges nothing, so it needs no payment
// method that can hold a change: it goes ahead after the customer moved
// to a bank debit, sent unprorated.
func TestSeats_KeepAfterMovingToBankDebit(t *testing.T) {
	f := newSeatFixture(t, 2)
	f.fake.failPayment = true
	up := f.preview(fmt.Sprintf(`{"license_id":%q,"quantity":6}`, f.lic.ID))
	if c, out := f.change(up, ""); c != http.StatusOK || envData(out)["status"] != "pending_payment" {
		t.Fatalf("held increase = %d %v", c, out)
	}
	f.fake.pmType = "sepa_debit"
	previews := len(f.fake.previews)
	keep := f.preview(fmt.Sprintf(`{"license_id":%q}`, f.lic.ID))
	if keep["total"] != float64(0) || keep["due_now"] != float64(0) || len(f.fake.previews) != previews {
		t.Fatalf("keep preview: total %v due %v, Stripe previews %d, want 0, 0 and none asked", keep["total"], keep["due_now"], len(f.fake.previews)-previews)
	}
	if c, out := f.change(keep, ""); c != http.StatusOK || envData(out)["status"] != "applied" || envData(out)["keep"] != true {
		t.Fatalf("keeping 2 seats after moving to bank debit = %d %v, want applied, keep", c, out)
	}
	u := f.fake.updates[len(f.fake.updates)-1]
	if u["proration_behavior"] != "none" || u["payment_behavior"] != "" || u["proration_date"] != "" {
		t.Fatalf("keep sent %v, want unprorated with no payment behaviour", u)
	}
	if l := f.license(); l.Quantity != 2 || l.StripePendingUpdateExpiresAt != nil {
		t.Fatalf("licence: quantity %d pending %v, want 2 and nothing waiting", l.Quantity, l.StripePendingUpdateExpiresAt)
	}
	// More seats by bank debit are still refused.
	if c, out := f.post(f.h.PreviewSubscriptionChange, f.lic.Email, fmt.Sprintf(`{"license_id":%q,"quantity":4}`, f.lic.ID)); c != http.StatusConflict || envCode(out) != "PAYMENT_METHOD_UNSUPPORTED" {
		t.Fatalf("more seats by bank debit = %d %v, want PAYMENT_METHOD_UNSUPPORTED", c, out)
	}
}

// The freeze on invites starts when the upgrade is written, after its
// checkout was opened, not when its transaction began: a slow Stripe
// call does not shorten it.
func TestUpgrade_FreezeStartsWhenTheUpgradeIsWritten(t *testing.T) {
	f := newUpgradeFixture(t, "upclock")
	var began, created time.Time
	err := f.s.WithXactLock(f.ctx, store.UpgradeLockKey(f.lic.ID), func(ctx context.Context, tx bun.Tx) error {
		if err := tx.NewRaw("SELECT now()").Scan(ctx, &began); err != nil {
			return err
		}
		if _, err := tx.NewRaw("SELECT pg_sleep(1)").Exec(ctx); err != nil {
			return err
		}
		u := &model.LicenseUpgrade{LicenseID: f.lic.ID, ToPlanID: f.paid.ID, StripeCheckoutSessionID: "cs_clock_" + f.tag}
		if err := store.CreateLicenseUpgradeIn(ctx, tx, u); err != nil {
			return err
		}
		created = u.CreatedAt
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Sub(began) < 900*time.Millisecond {
		t.Fatalf("created_at %v is %v after the transaction began, want the time it was written", created, created.Sub(began))
	}
}
