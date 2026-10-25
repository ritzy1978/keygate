package payment

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// invoice.paid extends a licence to the end of the period it pays for
// and never pulls it in: a mid-cycle invoice (a prorated upgrade or seat
// billed at once, a retried charge) has a period_end of the moment it was
// raised, and taking that would expire a customer who just paid.
func TestInvoicePaidNeverShortensValidUntil(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Invoice", Slug: "invoice-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Sub", Slug: "invoice-" + suffix, LicenseType: "subscription", LicenseModel: "standard", BillingInterval: "month"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	periodEnd := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	pastDue := time.Now().Add(-time.Hour)
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "invoice-" + suffix + "@example.com",
		LicenseKey: "KEY-invoice-" + suffix, Status: model.StatusPastDue, PastDueAt: &pastDue, ValidUntil: &periodEnd,
		StripeSubscriptionID: "sub_invoice_" + suffix}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	h := &StripeHandler{Store: s}
	validUntil := func() time.Time {
		t.Helper()
		var v time.Time
		if err := s.DB.NewRaw("SELECT valid_until FROM licenses WHERE id = ?", lic.ID).Scan(ctx, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	pay := func(body string) {
		t.Helper()
		h.onInvoicePaid(ctx, []byte(fmt.Sprintf(`{"subscription":%q,%s}`, lic.StripeSubscriptionID, body)))
	}

	// Mid-cycle: period_end is now, the proration line runs to the period
	// end; a one-off item's year of service is no licence period.
	pay(fmt.Sprintf(`"period_end":%d,"lines":{"data":[{"type":"invoiceitem","proration":true,"period":{"end":%d}},{"type":"invoiceitem","period":{"end":%d}}]}`,
		time.Now().Unix(), periodEnd.Unix(), time.Now().Add(365*24*time.Hour).Unix()))
	if got := validUntil(); !got.Equal(periodEnd) {
		t.Fatalf("mid-cycle invoice moved valid_until to %v, want %v", got, periodEnd)
	}
	var status string
	if err := s.DB.NewRaw("SELECT status FROM licenses WHERE id = ?", lic.ID).Scan(ctx, &status); err != nil || status != model.StatusActive {
		t.Fatalf("paid invoice did not reactivate: %s %v", status, err)
	}

	// An earlier period end and no lines: left alone.
	pay(fmt.Sprintf(`"period_end":%d`, time.Now().Unix()))
	if got := validUntil(); !got.Equal(periodEnd) {
		t.Fatalf("earlier period_end shortened valid_until to %v", got)
	}

	// No period at all: not set to the epoch.
	pay(`"period_end":0`)
	if got := validUntil(); !got.Equal(periodEnd) {
		t.Fatalf("invoice without a period moved valid_until to %v", got)
	}

	// The next cycle's invoice extends it.
	next := periodEnd.Add(30 * 24 * time.Hour)
	pay(fmt.Sprintf(`"period_end":%d,"lines":{"data":[{"type":"subscription","period":{"end":%d}}]}`, periodEnd.Unix(), next.Unix()))
	if got := validUntil(); !got.Equal(next) {
		t.Fatalf("renewal invoice: valid_until %v, want %v", got, next)
	}
}

// invoice.payment_failed reports each past_due episode exactly once: for
// a trial whose first charge fails, for repeated retries of the invoice,
// and when subscription.updated already moved the licence to past_due
// (Stripe sends the two in either order).
func TestPaymentFailedReportsEachEpisodeOnce(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Failed", Slug: "failed-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Trial", Slug: "failed-" + suffix, LicenseType: "trial", LicenseModel: "standard", TrialDays: 7}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	h := &StripeHandler{Store: s}
	until := time.Now().Add(time.Hour)
	mk := func(name, status string, pastDueAt *time.Time) *model.License {
		t.Helper()
		lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: name + "-" + suffix + "@example.com",
			LicenseKey: "KEY-" + name + "-" + suffix, Status: status, ValidUntil: &until, PastDueAt: pastDueAt,
			StripeSubscriptionID: "sub_" + name + "_" + suffix}
		if err := s.CreateLicense(ctx, lic); err != nil {
			t.Fatal(err)
		}
		return lic
	}
	fail := func(lic *model.License) {
		_ = h.onPaymentFailed(ctx, []byte(fmt.Sprintf(`{"subscription":%q,"status":"open"}`, lic.StripeSubscriptionID)))
	}
	// Audit lines are written in the background.
	audits := func(lic *model.License, want int) int {
		t.Helper()
		var n int
		for i := 0; i < 30; i++ {
			if err := s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = 'payment_failed'", lic.ID).Scan(ctx, &n); err != nil {
				t.Fatal(err)
			}
			if n >= want {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond) // and no more arrive
		_ = s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = 'payment_failed'", lic.ID).Scan(ctx, &n)
		return n
	}

	trial := mk("trial", model.StatusTrialing, nil)
	fail(trial)
	var status string
	var pastDue *time.Time
	if err := s.DB.NewRaw("SELECT status, past_due_at FROM licenses WHERE id = ?", trial.ID).Scan(ctx, &status, &pastDue); err != nil {
		t.Fatal(err)
	}
	if status != model.StatusPastDue || pastDue == nil {
		t.Fatalf("failed charge at the trial end: status=%s past_due_at=%v, want past_due", status, pastDue)
	}
	fail(trial) // Stripe's retry of the same invoice
	if n := audits(trial, 1); n != 1 {
		t.Fatalf("trial: %d payment_failed reports, want 1", n)
	}

	// subscription.updated got there first: past_due, nothing reported yet.
	entered := time.Now().Add(-time.Minute).Truncate(time.Second)
	moved := mk("moved", model.StatusPastDue, &entered)
	fail(moved)
	fail(moved)
	if n := audits(moved, 1); n != 1 {
		t.Fatalf("already past_due: %d payment_failed reports, want 1", n)
	}
}

// invoice.paid acts on the subscription as Stripe has it now, within what
// the licence's own state allows; subscription.resumed lifts only a pause
// Stripe made.
func TestInvoicePaidFollowsTheCurrentSubscription(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Current", Slug: "current-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Sub", Slug: "current-" + suffix, LicenseType: "subscription", LicenseModel: "standard", BillingInterval: "month"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(5 * 24 * time.Hour).Truncate(time.Second)
	periodEnd := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	stripeStatus := map[string]string{}
	mk := func(name, status, stripeSays string, setup string) *model.License {
		t.Helper()
		lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: name + "-" + suffix + "@example.com",
			LicenseKey: "KEY-" + name + "-" + suffix, Status: status, ValidUntil: &until, StripeSubscriptionID: "sub_" + name + "_" + suffix}
		if err := s.CreateLicense(ctx, lic); err != nil {
			t.Fatal(err)
		}
		if setup != "" {
			if _, err := s.DB.NewRaw("UPDATE licenses SET "+setup+" WHERE id = ?", lic.ID).Exec(ctx); err != nil {
				t.Fatal(err)
			}
		}
		stripeStatus[lic.StripeSubscriptionID] = stripeSays
		return lic
	}
	adminSuspended := mk("admsusp", model.StatusSuspended, "active", "suspended_by = 'admin', suspended_at = now(), past_due_at = now()")
	unpaid := mk("unpaid", model.StatusCanceled, "active", "canceled_at = now()")
	ended := mk("ended", model.StatusCanceled, "canceled", "canceled_at = now()")
	trial := mk("trialstart", model.StatusTrialing, "trialing", "")
	stripePaused := mk("stpaused", model.StatusSuspended, "active", "suspended_by = 'stripe', suspended_at = now()")
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[len("/v1/subscriptions/"):]
		fmt.Fprintf(w, `{"id":%q,"object":"subscription","status":%q,"cancel_at_period_end":false,"items":{"object":"list","data":[{"id":"si_1","current_period_end":%d}]}}`,
			id, stripeStatus[id], periodEnd.Unix())
	})
	h := &StripeHandler{Store: s}
	paid := func(lic *model.License) {
		t.Helper()
		if err := h.onInvoicePaid(ctx, []byte(fmt.Sprintf(`{"subscription":%q,"amount_paid":0,"period_end":%d}`, lic.StripeSubscriptionID, time.Now().Unix()))); err != nil {
			t.Fatal(err)
		}
	}
	state := func(lic *model.License) (string, time.Time) {
		t.Helper()
		var st string
		var vu time.Time
		if err := s.DB.NewRaw("SELECT status, valid_until FROM licenses WHERE id = ?", lic.ID).Scan(ctx, &st, &vu); err != nil {
			t.Fatal(err)
		}
		return st, vu
	}

	paid(adminSuspended)
	if st, vu := state(adminSuspended); st != model.StatusSuspended || !vu.Equal(until) {
		t.Fatalf("operator's suspension undone by invoice.paid: %s %v", st, vu)
	}
	if s.HasNotification(ctx, adminSuspended.ID, fmt.Sprintf("payment_recovered:%d", 0)) {
		t.Fatal("recovery notice sent for a refused write")
	}
	var notices int
	_ = s.DB.NewRaw("SELECT count(*) FROM notifications WHERE license_id = ? AND tag LIKE 'payment_recovered%'", adminSuspended.ID).Scan(ctx, &notices)
	if notices != 0 {
		t.Fatalf("%d recovery notices for a refused write", notices)
	}

	paid(unpaid)
	if st, vu := state(unpaid); st != model.StatusActive || !vu.Equal(periodEnd) {
		t.Fatalf("unpaid subscription paid up: %s %v, want active until the period end", st, vu)
	}

	paid(ended)
	if st, vu := state(ended); st != model.StatusCanceled || !vu.Equal(until) {
		t.Fatalf("ended subscription: %s %v, want canceled and the date untouched", st, vu)
	}

	paid(trial)
	if st, _ := state(trial); st != model.StatusTrialing {
		t.Fatalf("$0 invoice opening a trial: %s, want trialing", st)
	}

	resume := func(lic *model.License) {
		h.onSubscriptionResumed(ctx, []byte(fmt.Sprintf(`{"id":%q}`, lic.StripeSubscriptionID)))
	}
	resume(adminSuspended)
	if st, _ := state(adminSuspended); st != model.StatusSuspended {
		t.Fatalf("subscription.resumed lifted an operator's suspension: %s", st)
	}
	resume(stripePaused)
	var by string
	if err := s.DB.NewRaw("SELECT status || ':' || COALESCE(suspended_by, '') FROM licenses WHERE id = ?", stripePaused.ID).Scan(ctx, &by); err != nil {
		t.Fatal(err)
	}
	if by != "active:" {
		t.Fatalf("subscription.resumed after a Stripe pause: %s, want active with the marker cleared", by)
	}
}
