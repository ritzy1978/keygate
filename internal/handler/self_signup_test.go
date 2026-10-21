package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/config"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

func TestValidateSelfSignup(t *testing.T) {
	for _, tc := range []struct {
		name    string
		on      bool
		limit   int
		consent string
		price   string
		maint   maintenanceFields
		wantErr string
	}{
		{name: "off, anything goes", price: "price_1"},
		{name: "free plan", on: true, limit: 50, consent: "Send me product news"},
		{name: "priced plan", on: true, price: "price_1", wantErr: "without a stripe_price_id"},
		{name: "update period", on: true, maint: maintenanceFields{UpdatesDays: 365}, wantErr: "update period"},
		{name: "renewals", on: true, maint: maintenanceFields{StripeRenewalPriceID: "price_r"}, wantErr: "update period"},
		{name: "negative cap", limit: -1, wantErr: "negative"},
		{name: "long consent", consent: strings.Repeat("x", 501), wantErr: "500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSelfSignup(tc.on, tc.limit, tc.consent, tc.price, tc.maint)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error %v, want one mentioning %q", err, tc.wantErr)
			}
		})
	}
}

type signupFixture struct {
	t    *testing.T
	s    *store.Store
	ctx  context.Context
	h    *AuthHandler
	plan *model.Plan
}

// newSignupFixture opens the test database and seeds a free
// subscription plan; edit adjusts the plan before it is written.
func newSignupFixture(t *testing.T, edit func(p *model.Plan)) *signupFixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Signup", Slug: "signup-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	plan := &model.Plan{
		ProductID: prod.ID, Name: "Community", Slug: "community-" + suffix,
		LicenseType: "subscription", BillingInterval: "month", LicenseModel: "standard",
		MaxActivations: 1, SelfSignup: true,
	}
	if edit != nil {
		edit(plan)
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	h := &AuthHandler{Store: s, Config: &config.Config{JWTSecret: "test-secret", BaseURL: "http://localhost"}}
	return &signupFixture{t: t, s: s, ctx: ctx, h: h, plan: plan}
}

// code stores a fresh login code for email, as /send would.
func (f *signupFixture) code(email string) string {
	f.t.Helper()
	code := generateOTPCode()
	if err := f.s.CreateOTPCode(f.ctx, &model.OTPCode{
		Email: email, CodeHash: hashOTPCode(code), ExpiresAt: time.Now().Add(10 * time.Minute),
	}); err != nil {
		f.t.Fatalf("create otp: %v", err)
	}
	return code
}

func (f *signupFixture) call(fn gin.HandlerFunc, method string, body any) (*httptest.ResponseRecorder, map[string]any) {
	f.t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	c.Request = httptest.NewRequest(method, "/api/v1/signup/"+f.plan.CheckoutID, &buf)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "checkout_id", Value: f.plan.CheckoutID}}
	fn(c)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func (f *signupFixture) verify(email, code string, consent bool) (*httptest.ResponseRecorder, map[string]any) {
	return f.call(f.h.SignupVerify, http.MethodPost, map[string]any{
		"email": email, "code": code, "marketing_consent": consent,
	})
}

func (f *signupFixture) licences(email string) []model.License {
	f.t.Helper()
	var ls []model.License
	if err := f.s.DB.NewSelect().Model(&ls).
		Where("product_id = ?", f.plan.ProductID).Where("lower(email) = lower(?)", email).
		Scan(f.ctx); err != nil {
		f.t.Fatalf("list licences: %v", err)
	}
	return ls
}

func errorCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func dataField(out map[string]any, key string) any {
	d, _ := out["data"].(map[string]any)
	return d[key]
}

// A confirmed address gets exactly one self sign-up licence for the
// product; claiming again signs in to the same one, even after it has
// moved to another plan.
func TestSelfSignup_OneLicencePerAddress(t *testing.T) {
	f := newSignupFixture(t, nil)
	email := "Claim-" + f.plan.CheckoutID + "@example.com"
	lower := strings.ToLower(email)

	// With no mailer configured the code goes to the log; the reply is
	// the same either way.
	if w, out := f.call(f.h.SignupSend, http.MethodPost, map[string]any{"email": email}); w.Code != http.StatusOK || dataField(out, "status") != "sent" {
		t.Fatalf("send: %d %s", w.Code, w.Body.String())
	}

	w, out := f.verify(email, f.code(lower), true)
	if w.Code != http.StatusCreated || dataField(out, "status") != "created" {
		t.Fatalf("first claim: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Set-Cookie"), "session=") {
		t.Fatalf("first claim did not sign in: %v", w.Header().Values("Set-Cookie"))
	}
	ls := f.licences(email)
	if len(ls) != 1 {
		t.Fatalf("%d licences after the first claim, want 1", len(ls))
	}
	l := ls[0]
	if l.SignupSource != model.SignupSourceSelf || l.Status != model.StatusActive || l.UserID == "" {
		t.Fatalf("licence written wrong: source=%q status=%q user=%q", l.SignupSource, l.Status, l.UserID)
	}
	// The plan asks no consent question, so a true in the body is ignored.
	if l.MarketingConsent || l.MarketingConsentAt != nil {
		t.Fatalf("consent recorded without the form asking for it")
	}

	w, out = f.verify(email, f.code(lower), false)
	if w.Code != http.StatusOK || dataField(out, "status") != "existing" || dataField(out, "license_id") != l.ID {
		t.Fatalf("second claim: %d %s", w.Code, w.Body.String())
	}
	if n := len(f.licences(email)); n != 1 {
		t.Fatalf("%d licences after the second claim, want 1", n)
	}

	// Upgraded to a paid plan of the same product, the licence still
	// answers for the address: no second free one.
	paid := &model.Plan{
		ProductID: f.plan.ProductID, Name: "Premium", Slug: "premium-" + f.plan.CheckoutID,
		LicenseType: "subscription", BillingInterval: "month", LicenseModel: "standard", MaxActivations: 1,
	}
	if err := f.s.CreatePlan(f.ctx, paid); err != nil {
		t.Fatalf("create paid plan: %v", err)
	}
	if _, err := f.s.DB.NewUpdate().Model((*model.License)(nil)).Set("plan_id = ?", paid.ID).
		Where("id = ?", l.ID).Exec(f.ctx); err != nil {
		t.Fatalf("move licence: %v", err)
	}
	w, out = f.verify(email, f.code(lower), false)
	if w.Code != http.StatusOK || dataField(out, "status") != "existing" || dataField(out, "license_id") != l.ID {
		t.Fatalf("claim after the upgrade: %d %s", w.Code, w.Body.String())
	}
	if n := len(f.licences(email)); n != 1 {
		t.Fatalf("%d licences after the upgrade and a new claim, want 1", n)
	}
}

func TestSelfSignup_ConsentRecordedWhenAsked(t *testing.T) {
	f := newSignupFixture(t, func(p *model.Plan) { p.SignupConsentText = "Email me product news" })
	email := "consent-" + f.plan.CheckoutID + "@example.com"
	if w, _ := f.verify(email, f.code(email), true); w.Code != http.StatusCreated {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	l := f.licences(email)[0]
	if !l.MarketingConsent || l.MarketingConsentAt == nil {
		t.Fatalf("consent not recorded: %v %v", l.MarketingConsent, l.MarketingConsentAt)
	}

	_, out := f.call(f.h.SignupInfo, http.MethodGet, nil)
	if dataField(out, "consent_text") != "Email me product news" {
		t.Fatalf("info: %v", out)
	}
}

func TestSelfSignup_DailyCap(t *testing.T) {
	f := newSignupFixture(t, func(p *model.Plan) { p.SignupDailyCap = 1 })
	first := "cap1-" + f.plan.CheckoutID + "@example.com"
	if w, _ := f.verify(first, f.code(first), false); w.Code != http.StatusCreated {
		t.Fatalf("first claim: %d %s", w.Code, w.Body.String())
	}
	second := "cap2-" + f.plan.CheckoutID + "@example.com"
	w, out := f.verify(second, f.code(second), false)
	if w.Code != http.StatusTooManyRequests || errorCode(out) != "SIGNUP_LIMIT_REACHED" {
		t.Fatalf("claim over the cap: %d %s", w.Code, w.Body.String())
	}
	if n := len(f.licences(second)); n != 0 {
		t.Fatalf("claim over the cap wrote %d licences", n)
	}
	// The first address still signs in to its licence at the cap.
	if w, out := f.verify(first, f.code(first), false); w.Code != http.StatusOK || dataField(out, "status") != "existing" {
		t.Fatalf("existing claim at the cap: %d %s", w.Code, w.Body.String())
	}
}

func TestSelfSignup_CodeChecks(t *testing.T) {
	f := newSignupFixture(t, nil)
	email := "code-" + f.plan.CheckoutID + "@example.com"
	code := f.code(email)
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	if w, _ := f.verify(email, wrong, false); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong code: %d %s", w.Code, w.Body.String())
	}
	if n := len(f.licences(email)); n != 0 {
		t.Fatalf("wrong code wrote %d licences", n)
	}
	if w, _ := f.verify(email, code, false); w.Code != http.StatusCreated {
		t.Fatalf("right code: %d %s", w.Code, w.Body.String())
	}
	// A code works once.
	if w, _ := f.verify(email, code, false); w.Code != http.StatusUnauthorized {
		t.Fatalf("reused code: %d %s", w.Code, w.Body.String())
	}
}

// Plans that don't take self sign-ups all answer the same 404, on every
// step, whatever the reason.
func TestSelfSignup_UnavailablePlans(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(p *model.Plan)
	}{
		{"not opened", func(p *model.Plan) { p.SelfSignup = false }},
		{"priced", func(p *model.Plan) { p.StripePriceID = "price_signup_" + time.Now().Format("150405.000000") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSignupFixture(t, tc.edit)
			email := "closed-" + f.plan.CheckoutID + "@example.com"
			for _, step := range []struct {
				fn     gin.HandlerFunc
				method string
				body   any
			}{
				{f.h.SignupInfo, http.MethodGet, nil},
				{f.h.SignupSend, http.MethodPost, map[string]any{"email": email}},
				{f.h.SignupVerify, http.MethodPost, map[string]any{"email": email, "code": f.code(email)}},
			} {
				w, out := f.call(step.fn, step.method, step.body)
				if w.Code != http.StatusNotFound || errorCode(out) != "SIGNUP_UNAVAILABLE" {
					t.Fatalf("%s: %d %s", step.method, w.Code, w.Body.String())
				}
			}
			if n := len(f.licences(email)); n != 0 {
				t.Fatalf("closed plan wrote %d licences", n)
			}
		})
	}
}
