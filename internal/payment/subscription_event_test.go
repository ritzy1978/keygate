package payment

import "testing"

// Only a subscription that stops at the end of the current period is
// "ending": a cancel_at further out still renews this period.
func TestSubscriptionEventEndsThisPeriod(t *testing.T) {
	const periodEnd = int64(1_800_000_000)
	for _, tc := range []struct {
		name string
		ev   subscriptionEvent
		want bool
	}{
		{"renews", subscriptionEvent{CurrentPeriodEnd: periodEnd}, false},
		{"cancel_at_period_end", subscriptionEvent{CurrentPeriodEnd: periodEnd, CancelAtPeriodEnd: true}, true},
		{"cancel_at = period end", subscriptionEvent{CurrentPeriodEnd: periodEnd, CancelAt: periodEnd}, true},
		{"cancel_at inside the period", subscriptionEvent{CurrentPeriodEnd: periodEnd, CancelAt: periodEnd - 86400}, true},
		{"cancel_at in a later period", subscriptionEvent{CurrentPeriodEnd: periodEnd, CancelAt: periodEnd + 30*86400}, false},
		{"period end only on items", func() subscriptionEvent {
			var e subscriptionEvent
			e.CancelAt = periodEnd + 30*86400
			e.Items.Data = append(e.Items.Data, subscriptionItemEvent{CurrentPeriodEnd: periodEnd})
			return e
		}(), false},
		{"cancel_at, period end unknown", subscriptionEvent{CancelAt: periodEnd}, true},
		{"already canceled in Stripe", subscriptionEvent{Status: "canceled", CurrentPeriodEnd: periodEnd}, true},
		{"incomplete_expired", subscriptionEvent{Status: "incomplete_expired"}, true},
		{"past_due still renews", subscriptionEvent{Status: "past_due", CurrentPeriodEnd: periodEnd}, false},
	} {
		if got := tc.ev.EndsThisPeriod(); got != tc.want {
			t.Errorf("%s: EndsThisPeriod = %v, want %v", tc.name, got, tc.want)
		}
	}
}
