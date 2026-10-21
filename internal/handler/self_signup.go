package handler

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/license"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

// Self sign-up: a visitor claims a licence on a free plan by confirming
// an email address with a login code. The plan's /pay link leads here
// when the plan has self_signup on; the licence key is mailed like any
// other, and the visitor ends up signed in to the portal.
//
//	GET  /api/v1/signup/:checkout_id         what the form shows
//	POST /api/v1/signup/:checkout_id/send    {email}
//	POST /api/v1/signup/:checkout_id/verify  {email, code, marketing_consent}

// signupPlan returns the plan behind checkout_id when it takes self
// sign-ups. Otherwise it writes the 404 and returns nil: a plan that
// is unknown, inactive, priced or not opened all answer the same.
func (h *AuthHandler) signupPlan(c *gin.Context) *model.Plan {
	checkoutID := c.Param("checkout_id")
	if len(checkoutID) == 8 {
		plan, err := h.Store.FindPlanByCheckoutID(c, checkoutID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			response.Internal(c, err)
			return nil
		}
		if err == nil && selfSignupOpen(plan) {
			return plan
		}
	}
	response.Err(c, http.StatusNotFound, "SIGNUP_UNAVAILABLE", "sign-up is not available for this plan")
	return nil
}

// selfSignupOpen reports whether a visitor may claim a licence on plan
// right now. A plan with an update period never qualifies: the period
// needs the gating checks a sale gets, and a free plan has no use for it.
func selfSignupOpen(plan *model.Plan) bool {
	return plan.Active && plan.SelfSignup && plan.StripePriceID == "" &&
		plan.InitialUpdatesUntil(time.Now()) == nil
}

// SignupInfo handles GET /api/v1/signup/:checkout_id
func (h *AuthHandler) SignupInfo(c *gin.Context) {
	plan := h.signupPlan(c)
	if plan == nil {
		return
	}
	productName := ""
	if p, err := h.Store.FindProductByID(c, plan.ProductID); err == nil {
		productName = p.Name
	}
	response.OK(c, gin.H{
		"product_name": productName,
		"plan_name":    plan.Name,
		"license_type": plan.LicenseType,
		"trial_days":   plan.TrialDays,
		"consent_text": plan.SignupConsentText,
	})
}

// SignupSend handles POST /api/v1/signup/:checkout_id/send
func (h *AuthHandler) SignupSend(c *gin.Context) {
	var req struct {
		Email string `json:"email" binding:"required,email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "valid email is required")
		return
	}
	if h.signupPlan(c) == nil {
		return
	}
	// The same code, limits and mail as a login. An address that
	// already holds a licence on the plan gets one too: the verify step
	// signs it in to that licence, so this reply says nothing about
	// which addresses are customers.
	h.sendCode(c, strings.ToLower(strings.TrimSpace(req.Email)), false)
}

// SignupVerify handles POST /api/v1/signup/:checkout_id/verify
func (h *AuthHandler) SignupVerify(c *gin.Context) {
	var req struct {
		Email            string `json:"email" binding:"required,email"`
		Code             string `json:"code" binding:"required"`
		MarketingConsent bool   `json:"marketing_consent"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "email and code are required")
		return
	}
	plan := h.signupPlan(c)
	if plan == nil {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !h.checkCode(c, email, strings.TrimSpace(req.Code)) {
		return
	}

	user, err := h.loginUser(c, email)
	if err != nil {
		response.Internal(c, err)
		return
	}

	// One self sign-up licence per address per product: a second claim
	// signs in to the first (on whatever plan it is now) instead of
	// minting another.
	if existing, err := h.Store.FindSelfSignupLicense(c, plan.ProductID, email); err == nil {
		h.startSession(c, user)
		response.OK(c, gin.H{"status": "existing", "license_id": existing.ID})
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		response.Internal(c, err)
		return
	}

	status := model.StatusActive
	if plan.LicenseType == "trial" {
		status = model.StatusTrialing
	}
	l := &model.License{
		ProductID:       plan.ProductID,
		PlanID:          plan.ID,
		UserID:          user.ID,
		Email:           email,
		LicenseKey:      license.GenerateKey(""),
		Status:          status,
		UpdatesTermsSet: true,
	}
	if plan.LicenseType == "trial" && plan.TrialDays > 0 {
		until := time.Now().Add(time.Duration(plan.TrialDays) * 24 * time.Hour)
		l.ValidUntil = &until
	}
	// Consent counts only when the form asked for it.
	if plan.SignupConsentText != "" && req.MarketingConsent {
		now := time.Now()
		l.MarketingConsent = true
		l.MarketingConsentAt = &now
	}

	if err := h.Store.CreateSelfSignupLicense(c, l, plan); err != nil {
		switch {
		case errors.Is(err, store.ErrSelfSignupExists) || store.IsSelfSignupConflict(err):
			// Another confirmation for this address committed first.
			existing, ferr := h.Store.FindSelfSignupLicense(c, plan.ProductID, email)
			if ferr != nil {
				response.Internal(c, ferr)
				return
			}
			h.startSession(c, user)
			response.OK(c, gin.H{"status": "existing", "license_id": existing.ID})
		case errors.Is(err, store.ErrSelfSignupClosed):
			response.Err(c, http.StatusNotFound, "SIGNUP_UNAVAILABLE", "sign-up is not available for this plan")
		case errors.Is(err, store.ErrSignupCapReached):
			response.Err(c, http.StatusTooManyRequests, "SIGNUP_LIMIT_REACHED",
				"this plan has taken all the sign-ups it accepts today; try again tomorrow")
		case errors.Is(err, store.ErrPlanChanged):
			response.Conflict(c, "PLAN_CHANGED",
				"the plan's license type changed while this license was being created; reload and try again", nil)
		default:
			response.Internal(c, err)
		}
		return
	}

	h.mailSelfSignupLicense(c, l, plan)

	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: l.ID, Action: "created",
		ActorType: model.SignupSourceSelf, ActorID: user.ID, IPAddress: c.ClientIP(),
		Changes: map[string]any{
			"email": email, "plan_id": plan.ID, "marketing_consent": l.MarketingConsent,
		},
	})

	if h.Webhook != nil {
		h.Webhook.Dispatch(c, l.ProductID, model.EventLicenseCreated, map[string]any{
			"license_id": l.ID, "email": l.Email, "plan_id": l.PlanID,
			"signup_source": l.SignupSource, "marketing_consent": l.MarketingConsent,
		})
	}

	h.startSession(c, user)
	response.Created(c, gin.H{"status": "created", "license_id": l.ID})
}

// mailSelfSignupLicense queues the licence email, the same template a
// purchase gets. Queued rather than sent inline so an SMTP hiccup is
// retried instead of losing the key; the portal shows it either way.
func (h *AuthHandler) mailSelfSignupLicense(c *gin.Context, l *model.License, plan *model.Plan) {
	if h.Email == nil {
		return
	}
	productName, downloadURL := "", ""
	if p, err := h.Store.FindProductByID(c, l.ProductID); err == nil {
		productName, downloadURL = p.Name, p.DownloadURL
	}
	subject, body := h.Email.RenderLicenseCreated(productName, plan.Name, h.Store.DecryptLicenseKey(l), downloadURL)
	if err := h.Store.EnqueueEmail(c, l.Email, subject, body); err != nil {
		slog.Error("self sign-up: failed to queue licence email", "license_id", l.ID, "error", err)
	}
}
