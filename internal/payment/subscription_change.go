package payment

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stripe/stripe-go/v82"
	stripecustomer "github.com/stripe/stripe-go/v82/customer"
	stripeinvoice "github.com/stripe/stripe-go/v82/invoice"
	stripeprice "github.com/stripe/stripe-go/v82/price"
	"github.com/stripe/stripe-go/v82/subscription"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

// A customer changes what their subscription bills (its plan, its
// seats, or both) from the portal in two steps: a preview that shows
// what the change costs, and the change itself, which carries the
// preview's token so Stripe is asked for exactly what was shown.
//
// A change that lowers none of the licence's limits is invoiced at once,
// prorated (always_invoice), and held until that invoice is paid
// (pending_if_incomplete): more seats or a dearer plan apply only once
// paid. A change that lowers a limit is a downgrade: refused while more
// is in use than it leaves, it applies at once without proration, so
// nothing is invoiced now, the next invoice bills the new terms, and the
// rest of the period is not refunded. One change to a licence is with
// Stripe at a time, and while it is, nothing is taken from the licence's
// limits (store.LockLicenseChange). The licence takes the result from
// the subscription as Stripe returns it.

// changePreviewMaxAge is how long a preview may be acted on. Its
// proration date has to stay inside the billing period, and an amount
// shown an hour ago is not the amount the customer agreed to.
const changePreviewMaxAge = time.Hour

// subscriptionChange is a requested change, checked against the
// licence and the subscription as Stripe has them now.
type subscriptionChange struct {
	lic  *model.License
	sub  *stripe.Subscription
	item *stripe.SubscriptionItem
	from *model.Plan
	to   *model.Plan
	// price is the Stripe price the subscription will bill once
	// changed: the one it has when the plan stays, which may be an
	// older price of that plan, or the new plan's.
	price string
	// quantity is what the subscription will bill once changed.
	quantity int
	// done: the subscription already bills price at quantity (see
	// changeRequest.made).
	done bool
	// downgrade: the change lowers a limit the licence's product uses;
	// lowerSeats and lowerDevices are the limits it lowers to, nil for one
	// it leaves alone. floating: the device limit counts floating
	// sessions too.
	downgrade                bool
	lowerSeats, lowerDevices *int
	floating                 bool
	// mixed: a downgrade that also raises the other limit.
	mixed bool
	// keep: the terms the subscription has, asked for to drop a change
	// waiting for payment. It charges nothing.
	keep bool
	// retired: the subscription bills a price no plan sells any more, so
	// a read of it does not say which plan it is on.
	retired bool
	// people is who is on the licence now, the owner included; devices
	// how many machines are activated on it.
	people, devices int
}

// changeRequest names a change: a price to move to ("" keeps the
// current one) and a quantity (0 keeps the current one, or 1 on a plan
// not sold per seat).
type changeRequest struct {
	LicenseID string `json:"license_id" binding:"required"`
	PriceID   string `json:"price_id"`
	Quantity  int    `json:"quantity"`
	// made accepts a subscription that already is what was asked for:
	// a retry of a change whose answer was lost finds it made.
	made bool
}

// resolveChange checks a change the way both steps need it checked and
// writes the refusal itself; ok is false when it did. Only the licence's
// owner changes what it costs, as with renewals and upgrades.
func (h *StripeHandler) resolveChange(c *gin.Context, req changeRequest) (ch *subscriptionChange, ok bool) {
	lic, err := h.Store.FindLicenseByID(c, req.LicenseID)
	if err != nil {
		response.NotFound(c, "license not found")
		return nil, false
	}
	emailVal, _ := c.Get("email")
	if e, ok := emailVal.(string); !ok || !strings.EqualFold(lic.Email, e) {
		response.Forbidden(c, "not your license")
		return nil, false
	}
	if lic.StripeSubscriptionID == "" {
		response.BadRequest(c, "license has no Stripe subscription")
		return nil, false
	}
	// A licence behind on payment settles first: a smaller subscription
	// would be credited for time that was never paid for.
	if lic.Status != model.StatusActive && lic.Status != model.StatusTrialing {
		response.Conflict(c, "SUBSCRIPTION_NOT_ACTIVE",
			"this subscription can be changed once it is active again; settle any open payment first", nil)
		return nil, false
	}
	if req.Quantity < 0 {
		response.BadRequest(c, "quantity must be 1 or more")
		return nil, false
	}
	if stripe.Key == "" {
		response.Err(c, http.StatusServiceUnavailable, "PAYMENTS_UNAVAILABLE", "payments are not configured")
		return nil, false
	}

	get := &stripe.SubscriptionParams{}
	get.AddExpand("default_payment_method")
	get.AddExpand("customer.invoice_settings.default_payment_method")
	ctx, cancel := stripeContext(c)
	defer cancel()
	get.Context = ctx
	readAt, err := h.Store.StripeReadStamp(c)
	if err != nil {
		response.Internal(c, err)
		return nil, false
	}
	sub, err := subscription.Get(lic.StripeSubscriptionID, get)
	if err != nil {
		var serr *stripe.Error
		if errors.As(err, &serr) && serr.HTTPStatusCode == http.StatusTooManyRequests {
			c.Header("Retry-After", "5")
			response.Err(c, http.StatusTooManyRequests, "RATE_LIMITED", "Stripe is busy; try again in a moment")
			return nil, false
		}
		response.Internal(c, fmt.Errorf("read subscription %s: %w", lic.StripeSubscriptionID, err))
		return nil, false
	}
	switch {
	case sub.Status != stripe.SubscriptionStatusActive && sub.Status != stripe.SubscriptionStatusTrialing:
		response.Conflict(c, "SUBSCRIPTION_NOT_ACTIVE",
			"this subscription can be changed once it is active again; settle any open payment first", nil)
		return nil, false
	// A schedule moving to its next phase would undo a change. A
	// subscription the seller built by hand (several items, metered
	// usage) is theirs to change.
	case sub.Schedule != nil,
		sub.Items == nil || len(sub.Items.Data) != 1 || sub.Items.Data[0] == nil || sub.Items.Data[0].Price == nil,
		sub.Items.Data[0].Price.Recurring != nil && sub.Items.Data[0].Price.Recurring.UsageType == stripe.PriceRecurringUsageTypeMetered:
		response.Conflict(c, "CHANGE_NOT_SUPPORTED",
			"this subscription can't be changed here; please contact the seller", nil)
		return nil, false
	}
	item := sub.Items.Data[0]

	from, err := h.planForPrice(c, item.Price.ID)
	if err != nil {
		response.Internal(c, err)
		return nil, false
	}
	// A price no plan sells any more (the plan moved to a new price, and
	// this subscription kept the old one) is still the licence's plan.
	retired := false
	if (from == nil || from.ProductID != lic.ProductID) && lic.Plan != nil && lic.Plan.ProductID == lic.ProductID {
		from, retired = lic.Plan, true
	}
	if from == nil || from.ProductID != lic.ProductID {
		response.Conflict(c, "CHANGE_NOT_SUPPORTED",
			"this subscription can't be changed here; please contact the seller", nil)
		return nil, false
	}
	to, price := from, item.Price.ID
	if req.PriceID != "" && req.PriceID != item.Price.ID {
		to, err = h.Store.FindPlanByStripePrice(c, req.PriceID)
		if err != nil || to == nil {
			response.BadRequest(c, "invalid price_id")
			return nil, false
		}
		if to.ProductID != lic.ProductID {
			response.BadRequest(c, "new plan must belong to the same product")
			return nil, false
		}
		// A discontinued tier stays closed, and a subscription moves
		// between subscription prices only.
		if !to.Active {
			response.Err(c, http.StatusBadRequest, "PLAN_INACTIVE", "target plan is no longer available")
			return nil, false
		}
		if to.LicenseType != "subscription" {
			response.Err(c, http.StatusBadRequest, "NOT_SUBSCRIPTION_PLAN", "change-plan only accepts subscription plans")
			return nil, false
		}
		price = to.StripePriceID
	}

	quantity := req.Quantity
	if quantity == 0 {
		quantity = clampQuantity(item.Quantity)
	}
	// A plan not sold per seat takes no quantity from the customer. It
	// keeps the one the subscription has when that is not per seat
	// either (a quantity the seller set in Stripe), and is one coming
	// from a plan sold per seat.
	licenceQuantity := 1
	if !from.PerSeat {
		licenceQuantity = clampQuantity(item.Quantity)
	}
	switch {
	case !to.PerSeat && req.Quantity > 1 && req.Quantity != licenceQuantity:
		response.BadRequest(c, "this plan is not sold per seat")
		return nil, false
	case !to.PerSeat:
		quantity = licenceQuantity
	case price == item.Price.ID && int64(quantity) == item.Quantity:
		// The terms the subscription has: kept as they are (dropping a
		// change waiting for payment), whatever range the plan sells now.
	case quantity < to.MinQuantity || quantity > to.MaxQuantity:
		// Never moved into range silently: a plan whose minimum is above
		// what the customer has would raise their bill without asking.
		response.Conflict(c, "QUANTITY_OUT_OF_RANGE",
			fmt.Sprintf("%s is sold in %d to %d seats", to.Name, to.MinQuantity, to.MaxQuantity),
			gin.H{"min": to.MinQuantity, "max": to.MaxQuantity})
		return nil, false
	}
	// A downgrade whose outcome is not known yet holds the licence's
	// terms: until it is settled, only that change (sent again) goes on.
	// Another, checked against the subscription as Stripe still shows it,
	// would overwrite the hold that keeps the licence within what the
	// first may yet bill. Judged on the licence as stored, before the
	// sync below brings Stripe's terms into this copy of it.
	if held, err := h.Store.LicenseTermsHeld(c, lic.ID); err != nil {
		response.Internal(c, err)
		return nil, false
	} else if held && (to.ID != lic.PlanID || quantity != lic.Quantity) {
		changeInProgress(c)
		return nil, false
	}
	// A preview finds the licence saying otherwise than Stripe (a
	// downgrade whose outcome was never known left it lower, say): it is
	// brought into line first.
	if !req.made && (lic.PlanID != from.ID || lic.Quantity != clampQuantity(item.Quantity)) {
		if cur := subscriptionRead(sub); cur != nil {
			if err := h.syncFromCurrent(c, lic, cur, readAt, "portal"); err != nil {
				slog.Warn("stripe: licence not brought into line with its subscription", "license_id", lic.ID, "error", err)
			}
		}
	}
	// What the subscription has already. With a change waiting for
	// payment, asking for it again is how that change is dropped: Stripe
	// replaces a held change with any later one.
	done := price == item.Price.ID && int64(quantity) == item.Quantity && sub.PendingUpdate == nil
	if done && !req.made {
		response.Conflict(c, "NO_CHANGE", "this is what the subscription already has", nil)
		return nil, false
	}

	ch = &subscriptionChange{lic: lic, sub: sub, item: item, from: from, to: to, price: price, quantity: quantity, done: done, retired: retired}
	ch.keep = !done && price == item.Price.ID && int64(quantity) == item.Quantity
	used, err := h.Store.CountInUse(c, lic.ID)
	if err != nil {
		response.Internal(c, err)
		return nil, false
	}
	ch.people, ch.devices = used.People, used.Activations
	oldSeats, oldDevices := from.Limits(clampQuantity(item.Quantity))
	newSeats, newDevices := to.Limits(quantity)
	lowersSeats := model.ProductSupports(productType(lic), model.CapSeats) && newSeats > 0 && (oldSeats == 0 || newSeats < oldSeats)
	lowersDevices := model.ProductSupports(productType(lic), model.CapActivations) && newDevices < oldDevices
	ch.downgrade = lowersSeats || lowersDevices
	raisesSeats := model.ProductSupports(productType(lic), model.CapSeats) && oldSeats > 0 && (newSeats == 0 || newSeats > oldSeats)
	raisesDevices := model.ProductSupports(productType(lic), model.CapActivations) && newDevices > oldDevices
	ch.mixed = (lowersSeats && raisesDevices) || (lowersDevices && raisesSeats)
	ch.floating = to.LicenseModel == "floating"
	if lowersSeats {
		ch.lowerSeats = &newSeats
	}
	if lowersDevices {
		ch.lowerDevices = &newDevices
	}

	if done || ch.keep {
		// Made already (a retry finds it so), or the terms the
		// subscription has kept as they are: nothing to check.
		return ch, true
	}
	if !ch.downgrade {
		// A change charged now is held until its invoice is paid, which
		// Stripe does only for an invoice charged automatically to some
		// payment methods (cards, wallets, Link, PayPal and the like, not
		// bank debits). Otherwise the change could not wait for the money,
		// so it is not offered here.
		if sub.CollectionMethod != stripe.SubscriptionCollectionMethodChargeAutomatically || sub.PauseCollection != nil {
			response.Conflict(c, "CHANGE_NOT_SUPPORTED",
				"this subscription can't be changed here; please contact the seller", nil)
			return nil, false
		}
		if pm := defaultPaymentMethod(sub); pm != nil && !holdsChanges[pm.Type] {
			response.Conflict(c, "PAYMENT_METHOD_UNSUPPORTED",
				"this subscription's payment method can't hold a change until it is paid; use a card, or contact the seller", nil)
			return nil, false
		}
		return ch, true
	}
	if r := ch.overLimit(used); r != nil {
		r.write(c)
		return nil, false
	}
	return ch, true
}

// overLimit refuses a downgrade below what is in use. Nobody and nothing
// is removed by a change, so the owner frees the room first: otherwise
// seats bought for a moment would leave their people and devices behind
// for good. One that leaves a licence over a limit it was already over,
// but no further, goes ahead.
func (ch *subscriptionChange) overLimit(u store.InUse) *inUse {
	if s := ch.lowerSeats; s != nil && u.People > *s {
		return &inUse{code: "SEATS_IN_USE", inUse: u.People, limit: *s}
	}
	if d := ch.lowerDevices; d != nil {
		// Activations the owner can remove in the portal; floating
		// sessions end on their own when their machines stop.
		n, kind := u.Activations, "activations"
		if ch.floating && u.Sessions > *d && u.Sessions >= n {
			n, kind = u.Sessions, "sessions"
		}
		if n > *d {
			return &inUse{code: "DEVICES_IN_USE", kind: kind, inUse: n, limit: *d}
		}
	}
	return nil
}

// inUse is a downgrade's refusal: more in use than it leaves.
type inUse struct {
	code         string
	kind         string
	inUse, limit int
}

func (e *inUse) Error() string { return e.code }

func (e *inUse) write(c *gin.Context) {
	if e.code == "SEATS_IN_USE" {
		response.Conflict(c, e.code,
			fmt.Sprintf("%d people are on this license; remove %d before going down to %d seats", e.inUse, e.inUse-e.limit, e.limit),
			gin.H{"in_use": e.inUse, "seat_limit": e.limit})
		return
	}
	response.Conflict(c, e.code,
		fmt.Sprintf("%d %s in use on this license; free %d before going down to %d", e.inUse, e.kind, e.inUse-e.limit, e.limit),
		gin.H{"in_use": e.inUse, "activation_limit": e.limit, "kind": e.kind})
}

// holdsChanges are the payment methods Stripe holds a subscription
// change for until its invoice is paid (payment_behavior
// pending_if_incomplete); see Stripe's "pending updates" guide.
var holdsChanges = map[stripe.PaymentMethodType]bool{
	"card": true, "link": true, "alipay": true, "amazon_pay": true, "afterpay_clearpay": true,
	"cashapp": true, "eps": true, "gopay": true, "kakao_pay": true, "klarna": true, "kr_card": true,
	"naver_pay": true, "ng_card": true, "paypal": true, "payto": true, "pix": true, "promptpay": true,
	"revolut_pay": true, "satispay": true, "crypto": true, "swish": true, "twint": true, "upi": true,
	"wechat_pay": true,
}

// defaultPaymentMethod is the payment method a subscription charges:
// its own, or its customer's invoice default. nil when neither is known
// (an older card source, say), which is left to Stripe.
func defaultPaymentMethod(sub *stripe.Subscription) *stripe.PaymentMethod {
	if pm := sub.DefaultPaymentMethod; pm != nil && pm.Type != "" {
		return pm
	}
	if sub.Customer != nil && sub.Customer.InvoiceSettings != nil {
		if pm := sub.Customer.InvoiceSettings.DefaultPaymentMethod; pm != nil && pm.Type != "" {
			return pm
		}
	}
	return nil
}

// downgradeHoldTTL is how long a downgrade's lower terms hold against
// reads of the subscription that still show the old ones: past any
// request to Stripe, retries included, that may still make it.
const downgradeHoldTTL = 10 * time.Minute

// changeLockWait is how long a change waits for one already with
// Stripe for the same licence before it gives up.
const changeLockWait = 3 * time.Second

// changeStripeTimeout bounds each call a change makes to Stripe, retries
// included: another change to the licence waits for it.
const changeStripeTimeout = 30 * time.Second

// stripeContext is a context for a call to Stripe: bounded, and not cut
// off when the customer closes the page, which would leave the outcome
// unknown.
func stripeContext(c *gin.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(c), changeStripeTimeout)
}

// changeRetries is how many times a change Stripe gave no answer for is
// asked again, changeRetryWait apart, each bounded by
// changeRetryTimeout.
const (
	changeRetries      = 3
	changeRetryTimeout = 10 * time.Second
)

var changeRetryWait = 2 * time.Second

// changeSlots bounds the changes with Stripe at once: each holds a
// database connection (its licence's lock) for as long.
var changeSlots = make(chan struct{}, 8)

// changeInProgress refuses a change while another is with Stripe.
func changeInProgress(c *gin.Context) {
	response.Conflict(c, "CHANGE_IN_PROGRESS", "another change to this subscription is being confirmed; try again in a moment", nil)
}

// productType is the type of the licence's product, "" when it is not
// loaded.
func productType(lic *model.License) string {
	if lic.Product == nil {
		return ""
	}
	return lic.Product.Type
}

// downgradeAllowed says whether a downgrade can be billed from the next
// invoice. Not one that raises the other limit: the licence would take
// it before Stripe makes the change. Nor one to a price billed on another
// schedule: Stripe starts
// the new schedule, and invoices it, on the day of the change. Nor one
// that costs more for the rest of the period: unprorated, it would hand
// over a dearer plan for nothing, to be credited back at its price on
// the way back.
func (h *StripeHandler) downgradeAllowed(c *gin.Context, ch *subscriptionChange, prorationDate int64) (bool, error) {
	// Nor one to another plan from a price no plan sells any more: a read
	// of the subscription would not show which plan it ended on, so the
	// licence, moved first, could not be told it never moved.
	if ch.mixed || (ch.retired && ch.to.ID != ch.from.ID) {
		return false, nil
	}
	ctx, cancel := stripeContext(c)
	defer cancel()
	if ch.price != ch.item.Price.ID {
		target, err := stripeprice.Get(ch.price, &stripe.PriceParams{Params: stripe.Params{Context: ctx}})
		if err != nil {
			return false, err
		}
		from := ch.item.Price.Recurring
		if from == nil || target.Recurring == nil || from.Interval != target.Recurring.Interval ||
			from.IntervalCount != target.Recurring.IntervalCount {
			return false, nil
		}
	}
	inv, err := stripeinvoice.CreatePreview(&stripe.InvoiceCreatePreviewParams{
		Params:       stripe.Params{Context: ctx},
		Customer:     stripe.String(ch.sub.Customer.ID),
		Subscription: stripe.String(ch.sub.ID),
		SubscriptionDetails: &stripe.InvoiceCreatePreviewSubscriptionDetailsParams{
			Items: []*stripe.InvoiceCreatePreviewSubscriptionDetailsItemParams{{
				ID: stripe.String(ch.item.ID), Price: stripe.String(ch.price), Quantity: stripe.Int64(int64(ch.quantity)),
			}},
			ProrationBehavior: stripe.String("always_invoice"),
			ProrationDate:     stripe.Int64(prorationDate),
		},
	})
	if err != nil {
		return false, err
	}
	return inv.Total <= 0, nil
}

// downgradeRefused refuses a downgrade downgradeAllowed does not allow.
func downgradeRefused(c *gin.Context) {
	response.Conflict(c, "DOWNGRADE_NOT_SUPPORTED",
		"a change that includes less in one way but more in another, costs more, or bills on another schedule can't be made here; please contact the seller", nil)
}

// changeTerms is one side of a change, as the portal shows it: the
// plan, the Stripe price billed for it, and the seats.
func changeTerms(plan *model.Plan, priceID string, quantity int) gin.H {
	seats, activations := plan.Limits(quantity)
	return gin.H{
		"plan_id": plan.ID, "plan_name": plan.Name, "price_id": priceID,
		"per_seat": plan.PerSeat, "quantity": quantity,
		"seat_limit": seats, "activation_limit": activations,
	}
}

// PreviewSubscriptionChange shows what a change would cost: what is
// charged now (a credit when negative), and how much of the customer's
// Stripe balance would pay for it. Stripe's preview leaves the balance
// out, so it is read separately. Nothing changes in Stripe; a licence
// found out of line with its subscription is brought into line first.
//
// POST /portal/subscription/preview  { license_id, price_id?, quantity? }
func (h *StripeHandler) PreviewSubscriptionChange(c *gin.Context) {
	var req changeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_id is required")
		return
	}
	ch, ok := h.resolveChange(c, req)
	if !ok {
		return
	}
	prorationDate := time.Now().Unix()
	if ch.sub.Customer == nil {
		response.Internal(c, fmt.Errorf("subscription %s has no customer", ch.sub.ID))
		return
	}
	if ch.downgrade {
		allowed, err := h.downgradeAllowed(c, ch, prorationDate)
		if err != nil {
			previewFailed(c, err)
			return
		}
		if !allowed {
			downgradeRefused(c)
			return
		}
	}
	// A downgrade is not prorated: its preview is the next invoice, on
	// the new terms. Any other change's is what it charges now.
	details := &stripe.InvoiceCreatePreviewSubscriptionDetailsParams{
		Items: []*stripe.InvoiceCreatePreviewSubscriptionDetailsItemParams{{
			ID: stripe.String(ch.item.ID), Price: stripe.String(ch.price), Quantity: stripe.Int64(int64(ch.quantity)),
		}},
		ProrationBehavior: stripe.String("always_invoice"),
		ProrationDate:     stripe.Int64(prorationDate),
	}
	// A subscription set to end has no next invoice to show.
	ending := ch.sub.CancelAtPeriodEnd || (ch.sub.CancelAt > 0 && ch.sub.CancelAt <= ch.item.CurrentPeriodEnd)
	if ch.downgrade {
		details.ProrationBehavior, details.ProrationDate = stripe.String("none"), nil
	}
	inv := &stripe.Invoice{Currency: ch.item.Price.Currency}
	ctx, cancel := stripeContext(c)
	defer cancel()
	// Kept terms charge nothing: there is no invoice to preview.
	if !ch.keep && (!ch.downgrade || !ending) {
		var err error
		inv, err = stripeinvoice.CreatePreview(&stripe.InvoiceCreatePreviewParams{
			Params:              stripe.Params{Context: ctx},
			Customer:            stripe.String(ch.sub.Customer.ID),
			Subscription:        stripe.String(ch.sub.ID),
			SubscriptionDetails: details,
		})
		if err != nil {
			previewFailed(c, err)
			return
		}
	}
	// The customer's balance goes on the invoice too: negative is credit
	// held with the seller, positive is owed. Stripe's preview leaves it
	// out.
	balance := int64(0)
	if cust, err := stripecustomer.Get(ch.sub.Customer.ID, &stripe.CustomerParams{Params: stripe.Params{Context: ctx}}); err == nil {
		balance = cust.Balance
	} else {
		slog.Warn("stripe: customer balance unreadable for a change preview", "customer_id", ch.sub.Customer.ID, "error", err)
	}
	credit := max(-balance, 0)
	total, dueNow := inv.Total, max(inv.Total+balance, 0)
	if inv.Total <= 0 {
		dueNow = 0
	}
	out := gin.H{
		"current":        changeTerms(ch.from, ch.item.Price.ID, clampQuantity(ch.item.Quantity)),
		"target":         changeTerms(ch.to, ch.price, ch.quantity),
		"downgrade":      ch.downgrade,
		"currency":       inv.Currency,
		"proration_date": prorationDate,
		"seats_in_use":   ch.people,
		"devices_in_use": ch.devices,
		// A change submitted now replaces one still waiting for payment.
		"replaces_pending": ch.sub.PendingUpdate != nil,
	}
	if ch.downgrade {
		// Nothing now; the next invoice, when it falls due, bills this.
		total, dueNow = 0, 0
		if !ending {
			out["next_invoice_total"] = inv.Total
			out["next_invoice_at"] = ch.item.CurrentPeriodEnd
		}
	}
	// total is the change itself: charged now when positive, put on the
	// balance for later invoices when negative.
	out["total"], out["credit_balance"], out["due_now"] = total, credit, dueNow
	// What POST /portal/subscription/change makes: this change, from these
	// terms, at this proration date, signed so none of it can be altered
	// on the way back.
	out["preview_token"] = h.signChange(changeToken{
		License: ch.lic.ID, Price: ch.price, Quantity: ch.quantity,
		FromPrice: ch.item.Price.ID, FromQuantity: clampQuantity(ch.item.Quantity), ProrationDate: prorationDate,
		Downgrade: ch.downgrade,
	})
	response.OK(c, out)
}

// previewFailed answers a preview Stripe could not make.
func previewFailed(c *gin.Context, err error) {
	// Stripe looked at it and said no (another currency, an address tax
	// needs): the change cannot be made as it stands.
	var serr *stripe.Error
	if errors.As(err, &serr) && serr.HTTPStatusCode == http.StatusTooManyRequests {
		c.Header("Retry-After", "5")
		response.Err(c, http.StatusTooManyRequests, "RATE_LIMITED", "Stripe is busy; try again in a moment")
		return
	}
	if errors.As(err, &serr) && serr.HTTPStatusCode >= 400 && serr.HTTPStatusCode < 500 {
		response.Conflict(c, "CHANGE_REFUSED", "Stripe could not price this change: "+serr.Msg, nil)
		return
	}
	response.Internal(c, fmt.Errorf("preview subscription change: %w", err))
}

// changeToken is a previewed change: the licence, the price and seats it
// moves to, the price and seats it starts from, and the proration date
// Stripe's preview used.
type changeToken struct {
	License       string `json:"l"`
	Price         string `json:"p"`
	Quantity      int    `json:"q"`
	FromPrice     string `json:"fp"`
	FromQuantity  int    `json:"fq"`
	ProrationDate int64  `json:"d"`
	// Downgrade: the preview showed a downgrade, which is not prorated.
	Downgrade bool `json:"g,omitempty"`
}

// signChange seals a previewed change with the handler's key.
func (h *StripeHandler) signChange(t changeToken) string {
	payload, _ := json.Marshal(t)
	body := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, h.changeKey())
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// openChange reads a token signChange made; ok is false for anything
// else, altered or not.
func (h *StripeHandler) openChange(token string) (t changeToken, ok bool) {
	body, sig, found := strings.Cut(token, ".")
	if !found {
		return t, false
	}
	// Strict: one encoding per signature, so an altered one never reads
	// as the same.
	got, err := base64.RawURLEncoding.Strict().DecodeString(sig)
	if err != nil {
		return t, false
	}
	mac := hmac.New(sha256.New, h.changeKey())
	mac.Write([]byte(body))
	if !hmac.Equal(got, mac.Sum(nil)) {
		return t, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || json.Unmarshal(payload, &t) != nil {
		return t, false
	}
	return t, true
}

// changeKey is the key previews are signed with: ChangeKey, set from the
// server's secret so every replica agrees, or a key of this process's
// own where none was set (a test).
func (h *StripeHandler) changeKey() []byte {
	if len(h.ChangeKey) > 0 {
		return h.ChangeKey
	}
	h.changeKeyOnce.Do(func() {
		h.ownChangeKey = make([]byte, 32)
		_, _ = rand.Read(h.ownChangeKey)
	})
	return h.ownChangeKey
}

// ChangeSubscription makes a change the customer previewed, exactly as
// the preview's token says: its target, and its proration date, so
// Stripe charges what was shown. The token is checked against the
// subscription as it is now: if the price or seats it started from
// moved, or it is over an hour old, the customer is asked to look again
// rather than charged an amount they were not shown.
//
// POST /portal/subscription/change  { license_id, preview_token }
//
//	→ { status: applied | pending_payment | processing, ... }
func (h *StripeHandler) ChangeSubscription(c *gin.Context) {
	var req struct {
		LicenseID    string `json:"license_id" binding:"required"`
		PreviewToken string `json:"preview_token" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_id and preview_token are required")
		return
	}
	tok, ok := h.openChange(req.PreviewToken)
	if !ok || tok.License != req.LicenseID {
		response.BadRequest(c, "invalid preview_token")
		return
	}
	age := time.Since(time.Unix(tok.ProrationDate, 0))
	if age > changePreviewMaxAge || age < -time.Minute {
		response.Conflict(c, "PREVIEW_EXPIRED", "this price is out of date; please review the change again", nil)
		return
	}
	change := changeRequest{LicenseID: tok.License, PriceID: tok.Price, Quantity: tok.Quantity, made: true}
	// Prorated from the moment it was shown: a customer who waits pays for
	// the time waited, never less.
	out, ok := h.changeSubscription(c, change, tok.ProrationDate, func(ch *subscriptionChange) bool {
		if ch.done {
			return true // made already: nothing to check it against
		}
		if ch.item.Price.ID != tok.FromPrice || ch.item.Quantity != int64(tok.FromQuantity) {
			response.Conflict(c, "SUBSCRIPTION_CHANGED", "your subscription changed since this price was shown; please review the change again", nil)
			return false
		}
		// The subscription renewed since the preview: the price shown was
		// for a period that is over.
		// So did the plan's limits, if what was a downgrade no longer is
		// (or the other way round): it is billed another way.
		if ch.item.CurrentPeriodStart > tok.ProrationDate || ch.downgrade != tok.Downgrade {
			response.Conflict(c, "PREVIEW_EXPIRED", "this price is out of date; please review the change again", nil)
			return false
		}
		return true
	})
	if !ok {
		return
	}
	if out["status"] == changeProcessing {
		c.JSON(http.StatusAccepted, response.Envelope{Success: true, Data: out})
		return
	}
	response.OK(c, out)
}

// What came of a change (the "status" of its answer).
const (
	changeApplied        = "applied"
	changePendingPayment = "pending_payment"
	changeProcessing     = "processing"
)

// changeSubscription asks Stripe for a change and returns what came of
// it. ok is false when the change was refused, and the refusal has been
// written. check, when set, vets the resolved change before Stripe is
// asked, and writes its own refusal.
func (h *StripeHandler) changeSubscription(c *gin.Context, req changeRequest, prorationDate int64, check func(*subscriptionChange) bool) (out gin.H, ok bool) {
	ch, ok := h.resolveChange(c, req)
	if !ok || (check != nil && !check(ch)) {
		return nil, false
	}
	select {
	case changeSlots <- struct{}{}:
		defer func() { <-changeSlots }()
	default:
		c.Header("Retry-After", "5")
		response.Err(c, http.StatusTooManyRequests, "RATE_LIMITED", "too many changes at once; try again in a moment")
		return nil, false
	}
	// One change at a time for a licence, checked again under the lock
	// against the subscription and the licence as they are now: another
	// change may have been made in between.
	lock, err := h.Store.LockLicenseChange(c, ch.lic.ID, changeLockWait)
	if errors.Is(err, store.ErrChangeInProgress) {
		changeInProgress(c)
		return nil, false
	}
	if err != nil {
		response.Internal(c, err)
		return nil, false
	}
	var unlockOnce sync.Once
	unlock := func() { unlockOnce.Do(lock.Unlock) }
	defer unlock()
	if ch, ok = h.resolveChange(c, req); !ok || (check != nil && !check(ch)) {
		return nil, false
	}

	// read brings the licence up to date with the subscription as it is
	// now. nil when it could not be read.
	read := func() (*stripe.Subscription, time.Time) {
		at, err := h.Store.StripeReadStamp(c)
		if err != nil {
			return nil, at
		}
		get := &stripe.SubscriptionParams{}
		get.AddExpand("latest_invoice")
		ctx, cancel := stripeContext(c)
		defer cancel()
		get.Context = ctx
		sub, err := subscription.Get(ch.sub.ID, get)
		if err != nil {
			return nil, at
		}
		return sub, at
	}
	result := func(sub *stripe.Subscription, asOf time.Time) gin.H {
		if cur := subscriptionRead(sub); cur != nil {
			if err := h.syncFromCurrent(c, ch.lic, cur, asOf, "portal"); err != nil {
				slog.Warn("stripe: subscription change made, licence not updated yet; the next event will", "license_id", ch.lic.ID, "error", err)
			}
		}
		out := gin.H{"plan_id": ch.to.ID, "plan_name": ch.to.Name, "quantity": ch.quantity}
		switch {
		case sub.PendingUpdate != nil && pendingIs(sub.PendingUpdate, ch.price, ch.quantity):
			out["status"] = changePendingPayment
			out["expires_at"] = time.Unix(sub.PendingUpdate.ExpiresAt, 0).UTC().Format(time.RFC3339)
			if sub.LatestInvoice != nil {
				out["hosted_invoice_url"] = sub.LatestInvoice.HostedInvoiceURL
			}
		case sub.PendingUpdate == nil && itemIs(sub, ch.price, ch.quantity):
			out["status"] = changeApplied
		default:
			return nil
		}
		return out
	}
	audit := map[string]any{
		"from_plan_id": ch.from.ID, "to_plan_id": ch.to.ID,
		"from_quantity": clampQuantity(ch.item.Quantity), "to_quantity": ch.quantity,
		"provider": "stripe", "downgrade": ch.downgrade,
	}
	finish := func(out gin.H) (gin.H, bool) {
		out["downgrade"] = ch.downgrade
		if ch.keep {
			out["keep"] = true
		}
		audit["outcome"] = out["status"]
		h.Store.Audit(c, &model.AuditLog{Entity: "license", EntityID: ch.lic.ID, Action: "subscription_change_requested", ActorType: "user", Changes: audit})
		return out, true
	}

	if ch.done {
		// Made already, by this request's first try.
		sub, at := read()
		if sub == nil {
			return finish(gin.H{"status": changeApplied, "plan_id": ch.to.ID, "plan_name": ch.to.Name, "quantity": ch.quantity})
		}
		if out := result(sub, at); out != nil {
			return finish(out)
		}
		// And moved on since, a moment ago.
		response.Conflict(c, "SUBSCRIPTION_CHANGED", "your subscription changed since this price was shown; please review the change again", nil)
		return nil, false
	}

	// Checked here too: change-plan has no preview.
	if ch.downgrade {
		if ch.sub.Customer == nil {
			response.Internal(c, fmt.Errorf("subscription %s has no customer", ch.sub.ID))
			return nil, false
		}
		allowed, err := h.downgradeAllowed(c, ch, time.Now().Unix())
		if err != nil {
			previewFailed(c, err)
			return nil, false
		}
		if !allowed {
			downgradeRefused(c)
			return nil, false
		}
	}

	// A downgrade lowers the licence first, after one more count under
	// the licence row lock, so nothing is taken on the limits it removes
	// while Stripe makes it; and holds those terms, in the licence row,
	// against reads of the subscription that still show the old ones,
	// whatever becomes of this request. A change Stripe made ends the
	// hold; one it did not make puts the old terms back.
	lowered := store.LicenseTerms{PlanID: ch.to.ID, Quantity: ch.quantity}
	var prev store.LicenseTerms
	// Taken before Stripe is asked, so its answer is newer than it.
	asOf, err := h.Store.StripeReadStamp(c)
	if err != nil {
		response.Internal(c, err)
		return nil, false
	}
	// apply applies a read of the subscription taken after asOf.
	apply := func(sub *stripe.Subscription, asOf time.Time) {
		if cur := subscriptionRead(sub); cur != nil {
			if err := h.syncFromCurrent(c, ch.lic, cur, asOf, "portal"); err != nil {
				slog.Warn("stripe: licence not brought up to date with its subscription; the next event will", "license_id", ch.lic.ID, "error", err)
			}
		}
	}
	// settle gives the change lock back, then reads the subscription once
	// more and applies it: a read that came in while a downgrade's terms
	// were held (an edit in the Stripe dashboard, say) left them alone,
	// and this one sets them as Stripe has them now.
	settle := func() {
		unlock()
		if sub, at := read(); sub != nil {
			apply(sub, at)
		}
	}
	// markRead: once Stripe may have made a downgrade, reads that began
	// before may still show the old terms; they are out of date before
	// the hold can end.
	markRead := func() {
		if ch.downgrade {
			if err := h.Store.MarkLicenseRead(c, ch.lic.ID); err != nil {
				slog.Warn("stripe: licence read stamp not moved after a downgrade", "license_id", ch.lic.ID, "error", err)
			}
		}
	}
	if ch.downgrade {
		prev, err = h.Store.LowerLicenseTerms(c, ch.lic.ID, lowered, downgradeHoldTTL, func(u store.InUse) error {
			if r := ch.overLimit(u); r != nil {
				return r
			}
			return nil
		})
		var refused *inUse
		if errors.As(err, &refused) {
			settle()
			refused.write(c)
			return nil, false
		}
		if err != nil {
			settle()
			response.Internal(c, err)
			return nil, false
		}
	}
	// notMade puts back what a downgrade Stripe did not make replaced,
	// without announcing anything (nothing was).
	notMade := func() {
		// A licence lowered already, by an earlier try of this change
		// whose outcome was never known, has no terms of its own to go
		// back to here: it keeps its hold, which the read below or the
		// lapsed-hold sweep settles against what Stripe bills.
		if ch.downgrade && prev != lowered {
			if err := h.Store.RestoreLicenseTerms(c, ch.lic.ID, lowered, prev); err != nil {
				slog.Warn("stripe: licence terms not put back after a change not made; the next event will", "license_id", ch.lic.ID, "error", err)
			}
		}
		settle()
	}
	// announceLowered announces the plan and seats a downgrade moved the
	// licence to, from those it had: it took them before Stripe was
	// asked, so no sync that follows finds anything to announce. A
	// licence that had them already (an earlier try of the same change
	// moved it, and said so) has nothing to announce. Should Stripe turn
	// out not to have made it, the sync that shows so moves the licence
	// back, and announces that.
	announceLowered := func() {
		if ch.downgrade {
			after := *ch.lic
			after.PlanID, after.Quantity = lowered.PlanID, lowered.Quantity
			h.announceTermsChanged(c, &after, prev, []string{"plan_id", "quantity"}, "portal")
		}
	}
	made := func() {
		announceLowered()
		markRead()
		if ch.downgrade {
			if err := h.Store.ReleaseLicenseTerms(c, ch.lic.ID); err != nil {
				slog.Warn("stripe: downgrade's hold not ended; it lapses on its own", "license_id", ch.lic.ID, "error", err)
			}
		}
		settle()
	}
	// shows reports whether a read of the subscription has this change,
	// made or held for payment.
	shows := func(sub *stripe.Subscription) bool {
		if sub.PendingUpdate != nil {
			return pendingIs(sub.PendingUpdate, ch.price, ch.quantity)
		}
		return itemIs(sub, ch.price, ch.quantity)
	}

	params := &stripe.SubscriptionParams{
		Items: []*stripe.SubscriptionItemsParams{{
			ID: stripe.String(ch.item.ID), Price: stripe.String(ch.price), Quantity: stripe.Int64(int64(ch.quantity)),
		}},
	}
	if ch.downgrade || ch.keep {
		// Charged nothing now. Kept terms drop the waiting change: Stripe
		// discards a pending update when the subscription is updated.
		params.ProrationBehavior = stripe.String("none")
	} else {
		params.PaymentBehavior = stripe.String("pending_if_incomplete")
		params.ProrationBehavior = stripe.String("always_invoice")
		params.ProrationDate = stripe.Int64(prorationDate)
	}
	params.AddExpand("latest_invoice")
	// The same change asked again (a retried request) is the same request
	// to Stripe, which makes it once.
	params.SetIdempotencyKey(fmt.Sprintf("keygate-change:%s:%s:%d:%d", ch.lic.ID, ch.price, ch.quantity, prorationDate))
	update := func(timeout time.Duration) (*stripe.Subscription, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c), timeout)
		defer cancel()
		params.Context = ctx
		return subscription.Update(ch.sub.ID, params)
	}
	updated, stripeErr := update(changeStripeTimeout)
	// No answer to go by: an attempt may have been made, or still be
	// running. The same request again is answered with what came of it
	// (Stripe replays it) or makes it once.
	var serr *stripe.Error
	refused := func(err error) bool {
		return errors.As(err, &serr) && serr.HTTPStatusCode >= 400 && serr.HTTPStatusCode < 500 &&
			serr.HTTPStatusCode != http.StatusConflict && serr.HTTPStatusCode != http.StatusTooManyRequests
	}
	for i := 0; stripeErr != nil && !refused(stripeErr) && i < changeRetries; i++ {
		slog.Warn("stripe: subscription change outcome unknown; asking again", "license_id", ch.lic.ID, "error", stripeErr)
		time.Sleep(changeRetryWait)
		updated, stripeErr = update(changeRetryTimeout)
	}
	// A downgrade's terms stay held until its outcome is known: the
	// licence already has them if Stripe made the change, and gets the
	// old ones back by hand if not, so no read of the subscription that
	// lands meanwhile can set terms older than that.

	if stripeErr == nil {
		sub := updated
		if updated.LastResponse != nil && updated.LastResponse.Header.Get("Idempotent-Replayed") == "true" {
			// An answer Stripe replays for a key it has seen is the
			// subscription as it was then: it is read again.
			if sub, asOf = read(); sub == nil {
				// Made, at some point: settled by the next read.
				announceLowered()
				markRead()
				return h.changeProcessing(c, ch, audit)
			}
		}
		if shows(sub) {
			out := result(sub, asOf)
			made()
			return finish(out)
		}
		// A replay of a change made before, on a subscription that has
		// moved on since: this request changed nothing.
		notMade()
		response.Conflict(c, "SUBSCRIPTION_CHANGED", "your subscription changed since this price was shown; please review the change again", nil)
		return nil, false
	}
	if refused(stripeErr) {
		// Stripe looked at the change and said no: nothing happened.
		slog.Warn("stripe: subscription change refused", "license_id", ch.lic.ID, "error", stripeErr)
		notMade()
		response.Conflict(c, "CHANGE_REFUSED", "Stripe could not make this change: "+serr.Msg, nil)
		return nil, false
	}
	// Still no answer. An attempt whose answer was lost may have been
	// made: one look at the subscription tells.
	slog.Warn("stripe: subscription change outcome unknown", "license_id", ch.lic.ID, "error", stripeErr)
	markRead()
	sub, at := read()
	if sub != nil && shows(sub) {
		out := result(sub, at)
		made()
		return finish(out)
	}
	// Not shown, as of now; but a busy answer (429) may hide an earlier
	// attempt, retried within the client, that Stripe is still running,
	// so nothing is put back on it either.
	// A downgrade keeps the lower terms (Stripe may yet make it, and
	// nothing must be taken on the limits it removes) until the
	// subscription's next event, or the customer's next look, shows how
	// it went; any other change is left to those as well. The read is
	// still applied, its terms aside while they are held: a renewal it
	// shows is not to wait for the next event.
	if sub != nil {
		apply(sub, at)
	}
	announceLowered()
	return h.changeProcessing(c, ch, audit)
}

// changeProcessing answers a change Stripe gave no usable answer for:
// it may or may not have been made, and the subscription's next event
// settles it.
func (h *StripeHandler) changeProcessing(c *gin.Context, ch *subscriptionChange, audit map[string]any) (gin.H, bool) {
	audit["outcome"] = changeProcessing
	h.Store.Audit(c, &model.AuditLog{Entity: "license", EntityID: ch.lic.ID, Action: "subscription_change_requested", ActorType: "user", Changes: audit})
	return gin.H{"status": changeProcessing, "plan_id": ch.to.ID, "plan_name": ch.to.Name, "quantity": ch.quantity, "downgrade": ch.downgrade, "keep": ch.keep}, true
}

// itemIs reports whether a subscription bills price at quantity now.
func itemIs(sub *stripe.Subscription, price string, quantity int) bool {
	if sub.Items == nil || len(sub.Items.Data) == 0 || sub.Items.Data[0] == nil || sub.Items.Data[0].Price == nil {
		return false
	}
	it := sub.Items.Data[0]
	return it.Price.ID == price && it.Quantity == int64(quantity)
}

// pendingIs reports whether a held change is to price at quantity.
func pendingIs(p *stripe.SubscriptionPendingUpdate, price string, quantity int) bool {
	if len(p.SubscriptionItems) == 0 || p.SubscriptionItems[0] == nil || p.SubscriptionItems[0].Price == nil {
		return false
	}
	it := p.SubscriptionItems[0]
	return it.Price.ID == price && it.Quantity == int64(quantity)
}

// subscriptionRead turns a subscription the API returned into the shape
// the sync applies; nil when its body cannot be read back.
func subscriptionRead(sub *stripe.Subscription) *subscriptionEvent {
	if sub == nil || sub.LastResponse == nil {
		return nil
	}
	var cur subscriptionEvent
	if json.Unmarshal(sub.LastResponse.RawJSON, &cur) != nil || cur.ID != sub.ID {
		return nil
	}
	return &cur
}

// PendingSubscriptionChange says whether a change waits for payment and
// where the customer pays for it.
//
// POST /portal/subscription/pending  { license_id }
//
//	→ { status: none | pending_payment, hosted_invoice_url?, expires_at?, quantity?, plan_name? }
func (h *StripeHandler) PendingSubscriptionChange(c *gin.Context) {
	var req struct {
		LicenseID string `json:"license_id" binding:"required"`
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
	if e, ok := emailVal.(string); !ok || !strings.EqualFold(lic.Email, e) {
		response.Forbidden(c, "not your license")
		return
	}
	if lic.StripeSubscriptionID == "" || stripe.Key == "" {
		response.OK(c, gin.H{"status": "none"})
		return
	}
	params := &stripe.SubscriptionParams{}
	params.AddExpand("latest_invoice")
	ctx, cancel := stripeContext(c)
	defer cancel()
	params.Context = ctx
	asOf, err := h.Store.StripeReadStamp(c)
	if err != nil {
		response.Internal(c, err)
		return
	}
	sub, err := subscription.Get(lic.StripeSubscriptionID, params)
	if err != nil {
		var serr *stripe.Error
		if errors.As(err, &serr) && serr.HTTPStatusCode == http.StatusTooManyRequests {
			c.Header("Retry-After", "5")
			response.Err(c, http.StatusTooManyRequests, "RATE_LIMITED", "Stripe is busy; try again in a moment")
			return
		}
		response.Internal(c, fmt.Errorf("read subscription %s: %w", lic.StripeSubscriptionID, err))
		return
	}
	// The licence's copy of the pending state may lag; this read is
	// newer, so it is applied while here.
	if cur := subscriptionRead(sub); cur != nil {
		if err := h.syncFromCurrent(c, lic, cur, asOf, "portal"); err != nil {
			slog.Warn("stripe: pending change read, licence not updated", "license_id", lic.ID, "error", err)
		}
	}
	if sub.PendingUpdate == nil {
		response.OK(c, gin.H{"status": "none"})
		return
	}
	out := gin.H{
		"status":     "pending_payment",
		"expires_at": time.Unix(sub.PendingUpdate.ExpiresAt, 0).UTC().Format(time.RFC3339),
	}
	if sub.LatestInvoice != nil {
		out["hosted_invoice_url"] = sub.LatestInvoice.HostedInvoiceURL
	}
	if items := sub.PendingUpdate.SubscriptionItems; len(items) > 0 && items[0] != nil {
		out["quantity"] = clampQuantity(items[0].Quantity)
		if items[0].Price != nil {
			if plan, err := h.planForPrice(c, items[0].Price.ID); err == nil && plan != nil {
				out["plan_name"] = plan.Name
			}
		}
	}
	response.OK(c, out)
}
