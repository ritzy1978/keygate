package payment

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSubscription is one Stripe subscription with one item, behind a
// stub of the endpoints a subscription change uses: read, update (with
// pending_if_incomplete), the preview, and the customer's balance.
// Prices are per unit; an update that costs more than the customer's
// credit fails to pay while failPayment is set, and is then held as a
// pending update, as Stripe holds it.
type fakeSubscription struct {
	mu          sync.Mutex
	id          string
	customer    string
	price       string
	quantity    int64
	status      string
	collection  string
	metered     bool
	unit        map[string]int64
	balance     int64 // negative: credit
	failPayment bool
	// pending is the change held until paid; nil when none.
	pending *struct {
		price    string
		quantity int64
	}
	// updateErr answers an update with this HTTP status instead.
	updateErr int
	// pmType is the type of the subscription's default payment method;
	// "" leaves it unset. paused pauses collection. periodStart starts
	// the item's current period.
	pmType      string
	paused      bool
	periodStart int64
	// previewErr answers a preview with this HTTP status instead.
	previewErr int
	// delay holds every update this long, as a slow payment does.
	delay time.Duration
	// extra serves any other path the test needs (an invoice, say);
	// it reports whether it answered.
	extra    func(w http.ResponseWriter, r *http.Request) bool
	updates  []map[string]string
	previews []map[string]string
	// replies holds the answer given to each idempotency key, which
	// Stripe gives again, unchanged, to a request carrying the same key.
	replies map[string]string
	// sent holds the parameters each idempotency key came with: Stripe
	// refuses the key with others.
	sent map[string]string
	// ending sets the subscription to end with its period; cancelAt, at
	// that time.
	ending   bool
	cancelAt int64
	// interval is how often each price bills; "month" when unset.
	interval map[string]string
	// afterGet runs once, unlocked, after the next read of the
	// subscription is answered and before its caller sees it.
	afterGet  func()
	periodEnd int64
}

func newFakeSubscription(id, price string, quantity int64, unit map[string]int64) *fakeSubscription {
	return &fakeSubscription{
		id: id, customer: "cus_" + id, price: price, quantity: quantity, status: "active",
		collection: "charge_automatically", unit: unit, periodEnd: time.Now().Add(15 * 24 * time.Hour).Unix(),
	}
}

func (f *fakeSubscription) json(expandInvoice bool) string {
	usage := "licensed"
	if f.metered {
		usage = "metered"
	}
	sub := map[string]any{
		"id": f.id, "object": "subscription", "status": f.status, "customer": f.customer,
		"collection_method": f.collection, "current_period_end": f.periodEnd,
		"items": map[string]any{"object": "list", "data": []any{map[string]any{
			"id": "si_" + f.id, "object": "subscription_item", "quantity": f.quantity,
			"current_period_end": f.periodEnd, "current_period_start": f.periodStart,
			"price": map[string]any{"id": f.price, "object": "price", "recurring": map[string]any{"usage_type": usage, "interval": f.intervalOf(f.price), "interval_count": 1}},
		}}},
		"pending_update": nil,
		"latest_invoice": "in_" + f.id,
	}
	if f.pmType != "" {
		sub["default_payment_method"] = map[string]any{"id": "pm_" + f.id, "object": "payment_method", "type": f.pmType}
	}
	if f.paused {
		sub["pause_collection"] = map[string]any{"behavior": "keep_as_draft"}
	}
	if f.ending {
		sub["cancel_at_period_end"] = true
	}
	if f.cancelAt > 0 {
		sub["cancel_at"] = f.cancelAt
	}
	if f.pending != nil {
		sub["pending_update"] = map[string]any{
			"expires_at": time.Now().Add(23 * time.Hour).Unix(),
			"subscription_items": []any{map[string]any{
				"id": "si_" + f.id, "object": "subscription_item", "quantity": f.pending.quantity,
				"price": map[string]any{"id": f.pending.price, "object": "price"},
			}},
		}
	}
	if expandInvoice {
		status := "paid"
		if f.pending != nil {
			status = "open"
		}
		sub["latest_invoice"] = map[string]any{"id": "in_" + f.id, "object": "invoice", "status": status,
			"billing_reason": "subscription_update", "hosted_invoice_url": "https://invoice.stripe.test/" + f.id}
	}
	b, _ := json.Marshal(sub)
	return string(b)
}

// intervalOf is how often price bills.
func (f *fakeSubscription) intervalOf(price string) string {
	if i := f.interval[price]; i != "" {
		return i
	}
	return "month"
}

// cost is what moving to price × quantity charges now: half a period's
// difference, as if half the period were left.
func (f *fakeSubscription) cost(price string, quantity int64) int64 {
	return (f.unit[price]*quantity - f.unit[f.price]*f.quantity) / 2
}

func (f *fakeSubscription) serve(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = r.ParseForm()
		form := map[string]string{}
		for k, v := range r.PostForm {
			form[k] = v[0]
		}
		expand := strings.Contains(r.URL.RawQuery, "latest_invoice") || strings.Contains(r.PostForm.Get("expand[0]"), "latest_invoice")
		switch {
		case r.URL.Path == "/v1/subscriptions/"+f.id && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(f.json(expand)))
			if hook := f.afterGet; hook != nil {
				f.afterGet = nil
				f.mu.Unlock()
				hook()
				f.mu.Lock()
			}
		case r.URL.Path == "/v1/subscriptions/"+f.id && r.Method == http.MethodPost:
			if f.delay > 0 {
				f.mu.Unlock()
				time.Sleep(f.delay)
				f.mu.Lock()
			}
			key := r.Header.Get("Idempotency-Key")
			form["_idempotency_key"] = key
			f.updates = append(f.updates, form)
			params := fmt.Sprint(r.PostForm)
			if was, seen := f.sent[key]; seen && key != "" && was != params {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"type":"idempotency_error","message":"Keys for idempotent requests can only be used with the same parameters they were first used with."}}`))
				return
			}
			if body, seen := f.replies[key]; seen && key != "" {
				w.Header().Set("Idempotent-Replayed", "true")
				_, _ = w.Write([]byte(body))
				return
			}
			if f.updateErr != 0 {
				w.WriteHeader(f.updateErr)
				_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"refused by the fake"}}`))
				return
			}
			price, quantity := f.price, f.quantity
			if p := form["items[0][price]"]; p != "" {
				price = p
			}
			if q := form["items[0][quantity]"]; q != "" {
				quantity, _ = strconv.ParseInt(q, 10, 64)
			}
			due := f.cost(price, quantity)
			if form["proration_behavior"] == "none" {
				due = 0 // nothing invoiced now; the next invoice bills it
			}
			if due+f.balance > 0 && f.failPayment && form["payment_behavior"] == "pending_if_incomplete" {
				f.pending = &struct {
					price    string
					quantity int64
				}{price, quantity}
			} else {
				f.balance += due
				if f.balance > 0 {
					f.balance = 0
				}
				f.price, f.quantity, f.pending = price, quantity, nil
			}
			body := f.json(expand)
			if key != "" {
				if f.replies == nil {
					f.replies, f.sent = map[string]string{}, map[string]string{}
				}
				f.replies[key], f.sent[key] = body, params
			}
			_, _ = w.Write([]byte(body))
		case r.URL.Path == "/v1/invoices/create_preview":
			f.previews = append(f.previews, form)
			if f.previewErr != 0 {
				w.WriteHeader(f.previewErr)
				_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"currency mismatch in the fake"}}`))
				return
			}
			price, quantity := f.price, f.quantity
			if p := form["subscription_details[items][0][price]"]; p != "" {
				price = p
			}
			if q := form["subscription_details[items][0][quantity]"]; q != "" {
				quantity, _ = strconv.ParseInt(q, 10, 64)
			}
			total := f.cost(price, quantity)
			if form["subscription_details[proration_behavior]"] == "none" {
				total = f.unit[price] * quantity // the next invoice, on these terms
			}
			_, _ = fmt.Fprintf(w, `{"object":"invoice","total":%d,"amount_due":%d,"currency":"usd","lines":{"object":"list","data":[]}}`, total, max(total, 0))
		case strings.HasPrefix(r.URL.Path, "/v1/prices/"):
			if f.extra != nil && f.extra(w, r) {
				return
			}
			id := strings.TrimPrefix(r.URL.Path, "/v1/prices/")
			_, _ = fmt.Fprintf(w, `{"id":%q,"object":"price","type":"recurring","unit_amount":%d,"recurring":{"interval":%q,"interval_count":1,"usage_type":"licensed"}}`,
				id, f.unit[id], f.intervalOf(id))
		case r.URL.Path == "/v1/customers/"+f.customer:
			_, _ = fmt.Fprintf(w, `{"id":%q,"object":"customer","balance":%d}`, f.customer, f.balance)
		default:
			if f.extra != nil && f.extra(w, r) {
				return
			}
			t.Errorf("unexpected Stripe call %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}
}

// pay settles the held change, as paying its invoice does in Stripe.
func (f *fakeSubscription) pay() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pending != nil {
		f.price, f.quantity, f.pending = f.pending.price, f.pending.quantity, nil
	}
}

// set changes the subscription behind Keygate's back, as an edit in the
// Stripe dashboard does.
func (f *fakeSubscription) set(price string, quantity int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.price, f.quantity = price, quantity
}

// hold makes Stripe hold a change for payment, as one made elsewhere
// (the dashboard, another replica) does.
func (f *fakeSubscription) hold(price string, quantity int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = &struct {
		price    string
		quantity int64
	}{price, quantity}
}
