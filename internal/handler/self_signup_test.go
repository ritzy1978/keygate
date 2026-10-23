package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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

// verify claims with the consent wording the fixture's plan was
// created with, as the form would send it.
func (f *signupFixture) verify(email, code string, consent bool) (*httptest.ResponseRecorder, map[string]any) {
	return f.verifyShown(email, code, consent, f.plan.SignupConsentText)
}

func (f *signupFixture) verifyShown(email, code string, consent bool, shown string) (*httptest.ResponseRecorder, map[string]any) {
	return f.call(f.h.SignupVerify, http.MethodPost, map[string]any{
		"email": email, "code": code, "marketing_consent": consent, "consent_text": shown,
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

func (f *signupFixture) hasAccount(email string) bool {
	f.t.Helper()
	n, err := f.s.DB.NewSelect().Model((*model.User)(nil)).Where("email = ?", strings.ToLower(email)).Count(f.ctx)
	if err != nil {
		f.t.Fatalf("count users: %v", err)
	}
	return n > 0
}

// skipNearUTCMidnight skips a test that counts today's claims when the
// UTC day could turn while it runs.
func skipNearUTCMidnight(t *testing.T) {
	t.Helper()
	now := time.Now().UTC()
	if now.Truncate(24*time.Hour).Add(24*time.Hour).Sub(now) < time.Minute {
		t.Skip("too close to UTC midnight for a daily count")
	}
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
	if w.Code != http.StatusOK || dataField(out, "status") != "existing" || dataField(out, "license_id") != l.ID ||
		dataField(out, "license_usable") != true {
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
	skipNearUTCMidnight(t)
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
	// A refused claim leaves no account behind: under
	// signup_mode=licensed_only an account is enough to log in.
	if f.hasAccount(second) {
		t.Fatal("claim over the cap created an account")
	}
	// Nor is a code mailed for a plan that is full.
	third := "cap3-" + f.plan.CheckoutID + "@example.com"
	if w, out := f.call(f.h.SignupSend, http.MethodPost, map[string]any{"email": third}); w.Code != http.StatusTooManyRequests || errorCode(out) != "SIGNUP_LIMIT_REACHED" {
		t.Fatalf("send at the cap: %d %s", w.Code, w.Body.String())
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
	// A self sign-up plan with a price cannot be stored at all (the
	// plans_self_signup_free constraint), so it is not a case here.
	for _, tc := range []struct {
		name string
		edit func(p *model.Plan)
		set  string
	}{
		{name: "not opened", edit: func(p *model.Plan) { p.SelfSignup = false }},
		{name: "inactive", set: "active = false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSignupFixture(t, tc.edit)
			if tc.set != "" {
				if _, err := f.s.DB.NewUpdate().Model((*model.Plan)(nil)).Set(tc.set).
					Where("id = ?", f.plan.ID).Exec(f.ctx); err != nil {
					t.Fatalf("edit plan: %v", err)
				}
			}
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

// Several requests presenting the same code at once: one claims it,
// the rest are turned away. The hook holds every request that matched
// until all have, so they all reach the consume together.
func TestSelfSignup_CodeUsedOnceUnderConcurrency(t *testing.T) {
	f := newSignupFixture(t, nil)
	email := "race-" + f.plan.CheckoutID + "@example.com"
	code := f.code(email)

	const n = 5
	var matched sync.WaitGroup
	matched.Add(n)
	var reached atomic.Int32
	f.h.beforeConsumeCode = func() {
		reached.Add(1)
		matched.Done()
		all := make(chan struct{})
		go func() { matched.Wait(); close(all) }()
		select {
		case <-all:
		case <-time.After(10 * time.Second):
		}
	}

	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w, _ := f.verify(email, code, false)
			codes[i] = w.Code
		}()
	}
	wg.Wait()

	if got := reached.Load(); got != n {
		t.Fatalf("%d of %d requests matched the code before any used it; the test proves nothing", got, n)
	}
	ok := 0
	for _, c := range codes {
		if c < 300 {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("%d requests got in with one code, want 1 (statuses %v)", ok, codes)
	}
	if got := len(f.licences(email)); got != 1 {
		t.Fatalf("%d licences, want 1", got)
	}
}

// Requests that both pass the compare can still only use a code once:
// the second consume finds it used.
func TestConsumeOTPCode_Once(t *testing.T) {
	f := newSignupFixture(t, nil)
	email := "consume-" + f.plan.CheckoutID + "@example.com"
	f.code(email)
	otp, err := f.s.FindLatestValidOTPCode(f.ctx, email)
	if err != nil {
		t.Fatalf("find code: %v", err)
	}
	for i, want := range []bool{true, false} {
		got, err := f.s.ConsumeOTPCode(f.ctx, otp.ID)
		if err != nil || got != want {
			t.Fatalf("consume #%d = %v, %v; want %v", i+1, got, err, want)
		}
	}
}

// Claims racing for the same address get one licence between them,
// and claims racing for the last place under the cap get one between
// them.
func TestSelfSignup_ConcurrentClaims(t *testing.T) {
	claim := func(f *signupFixture, email string) error {
		return f.s.CreateSelfSignupLicense(f.ctx, &model.License{
			ProductID: f.plan.ProductID, PlanID: f.plan.ID, Email: email,
			LicenseKey: "KG-RACE-" + email, Status: model.StatusActive, UpdatesTermsSet: true,
		}, f.plan)
	}
	race := func(n int, try func(i int) error) []error {
		errs := make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = try(i)
			}()
		}
		close(start)
		wg.Wait()
		return errs
	}

	t.Run("same address", func(t *testing.T) {
		f := newSignupFixture(t, nil)
		email := "same-" + f.plan.CheckoutID + "@example.com"
		created := 0
		for _, err := range race(6, func(int) error { return claim(f, email) }) {
			switch {
			case err == nil:
				created++
			case errors.Is(err, store.ErrSelfSignupExists) || store.IsSelfSignupConflict(err):
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		}
		if created != 1 || len(f.licences(email)) != 1 {
			t.Fatalf("created %d, stored %d, want 1 and 1", created, len(f.licences(email)))
		}
	})

	t.Run("last place under the cap", func(t *testing.T) {
		skipNearUTCMidnight(t)
		f := newSignupFixture(t, func(p *model.Plan) { p.SignupDailyCap = 1 })
		created := 0
		for _, err := range race(6, func(i int) error {
			return claim(f, "cap-"+f.plan.CheckoutID+"-"+string(rune('a'+i))+"@example.com")
		}) {
			switch {
			case err == nil:
				created++
			case errors.Is(err, store.ErrSignupCapReached):
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		}
		if n, err := f.s.CountSelfSignupsToday(f.ctx, f.plan.ID); err != nil || created != 1 || n != 1 {
			t.Fatalf("created %d, counted %d (%v), want 1 and 1", created, n, err)
		}
	})
}

// A plan deactivated after the handler read it takes no more claims.
func TestSelfSignup_DeactivatedMidClaim(t *testing.T) {
	f := newSignupFixture(t, nil)
	if _, err := f.s.DB.NewUpdate().Model((*model.Plan)(nil)).Set("active = false").
		Where("id = ?", f.plan.ID).Exec(f.ctx); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	err := f.s.CreateSelfSignupLicense(f.ctx, &model.License{
		ProductID: f.plan.ProductID, PlanID: f.plan.ID, Email: "late-" + f.plan.CheckoutID + "@example.com",
		LicenseKey: "KG-LATE-" + f.plan.CheckoutID, Status: model.StatusActive, UpdatesTermsSet: true,
	}, f.plan)
	if !errors.Is(err, store.ErrSelfSignupClosed) {
		t.Fatalf("claim on a deactivated plan: %v, want ErrSelfSignupClosed", err)
	}
}

// The address becomes a licence's email, so it is held to the admin
// form's rules.
func TestSelfSignup_RejectsMalformedEmail(t *testing.T) {
	f := newSignupFixture(t, nil)
	// Long enough to fail ValidateEmail while gin's email binding takes it.
	email := strings.Repeat("a", 250) + "@example.com"
	if w, _ := f.call(f.h.SignupSend, http.MethodPost, map[string]any{"email": email}); w.Code != http.StatusBadRequest {
		t.Fatalf("send: %d %s", w.Code, w.Body.String())
	}
	if w, _ := f.verify(email, "123456", false); w.Code != http.StatusBadRequest {
		t.Fatalf("verify: %d %s", w.Code, w.Body.String())
	}
}

// The cap counts claims made on the plan today, including one that has
// since moved to another plan.
func TestSelfSignup_CapCountsClaimsThatMoved(t *testing.T) {
	skipNearUTCMidnight(t)
	f := newSignupFixture(t, func(p *model.Plan) { p.SignupDailyCap = 1 })
	first := "moved-" + f.plan.CheckoutID + "@example.com"
	if w, _ := f.verify(first, f.code(first), false); w.Code != http.StatusCreated {
		t.Fatalf("first claim: %d %s", w.Code, w.Body.String())
	}
	other := &model.Plan{
		ProductID: f.plan.ProductID, Name: "Other", Slug: "other-" + f.plan.CheckoutID,
		LicenseType: "subscription", BillingInterval: "month", LicenseModel: "standard", MaxActivations: 1,
	}
	if err := f.s.CreatePlan(f.ctx, other); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if _, err := f.s.DB.NewUpdate().Model((*model.License)(nil)).Set("plan_id = ?", other.ID).
		Where("id = ?", f.licences(first)[0].ID).Exec(f.ctx); err != nil {
		t.Fatalf("move licence: %v", err)
	}
	second := "moved2-" + f.plan.CheckoutID + "@example.com"
	if w, out := f.verify(second, f.code(second), false); w.Code != http.StatusTooManyRequests || errorCode(out) != "SIGNUP_LIMIT_REACHED" {
		t.Fatalf("claim after the first moved away: %d %s", w.Code, w.Body.String())
	}
}

// A yes is recorded only against the wording the visitor saw. When the
// operator edits it mid sign-up, the licence is still created, without
// consent.
func TestSelfSignup_ConsentWordingChanged(t *testing.T) {
	f := newSignupFixture(t, func(p *model.Plan) { p.SignupConsentText = "Email me product news" })
	if _, err := f.s.DB.NewUpdate().Model((*model.Plan)(nil)).Set("signup_consent_text = ?", "Share my email with partners").
		Where("id = ?", f.plan.ID).Exec(f.ctx); err != nil {
		t.Fatalf("edit wording: %v", err)
	}
	old := "old-" + f.plan.CheckoutID + "@example.com"
	if w, _ := f.verifyShown(old, f.code(old), true, "Email me product news"); w.Code != http.StatusCreated {
		t.Fatalf("yes to the old wording: %d %s", w.Code, w.Body.String())
	}
	if l := f.licences(old)[0]; l.MarketingConsent || l.MarketingConsentAt != nil {
		t.Fatal("a yes to the old wording was recorded against the new one")
	}

	current := "new-" + f.plan.CheckoutID + "@example.com"
	if w, _ := f.verifyShown(current, f.code(current), true, "Share my email with partners"); w.Code != http.StatusCreated {
		t.Fatalf("yes to the current wording: %d %s", w.Code, w.Body.String())
	}
	if l := f.licences(current)[0]; !l.MarketingConsent {
		t.Fatal("a yes to the current wording was not recorded")
	}
}

// A claim on a licence that was revoked tells the form so.
func TestSelfSignup_ExistingReportsItsStatus(t *testing.T) {
	f := newSignupFixture(t, nil)
	email := "revoked-" + f.plan.CheckoutID + "@example.com"
	if w, _ := f.verify(email, f.code(email), false); w.Code != http.StatusCreated {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	if _, err := f.s.DB.NewUpdate().Model((*model.License)(nil)).Set("status = ?", model.StatusRevoked).
		Where("id = ?", f.licences(email)[0].ID).Exec(f.ctx); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	w, out := f.verify(email, f.code(email), false)
	if w.Code != http.StatusOK || dataField(out, "status") != "existing" ||
		dataField(out, "license_status") != model.StatusRevoked || dataField(out, "license_usable") != false {
		t.Fatalf("claim on a revoked licence: %d %s", w.Code, w.Body.String())
	}
}

// Whatever the handlers checked, the database refuses a self sign-up
// plan with a price: two admin edits racing (one turning sign-up on,
// one adding the price) would otherwise each pass and leave the pair.
// The constraint covers update periods and renewals the same way.
func TestSelfSignup_PlanConstraint(t *testing.T) {
	f := newSignupFixture(t, nil)
	_, err := f.s.DB.NewUpdate().Model((*model.Plan)(nil)).Set("stripe_price_id = ?", "price_race_"+f.plan.CheckoutID).
		Where("id = ?", f.plan.ID).Exec(f.ctx)
	if !store.IsSelfSignupPlanConflict(err) {
		t.Fatalf("adding a price to a self sign-up plan: %v, want the constraint", err)
	}
}

// Two admin edits racing: this one turns self sign-up on, and another
// adds a price after this one has checked the plan. The write is
// refused with the same 400 the check gives, not a 500.
func TestUpdatePlan_SelfSignupRacingAPrice(t *testing.T) {
	f := newSignupFixture(t, func(p *model.Plan) { p.SelfSignup = false })
	h := &AdminHandler{Store: f.s, beforeCutoffWrite: func() {
		if _, err := f.s.DB.NewUpdate().Model((*model.Plan)(nil)).Set("stripe_price_id = ?", "price_race_"+f.plan.CheckoutID).
			Where("id = ?", f.plan.ID).Exec(f.ctx); err != nil {
			t.Errorf("concurrent price: %v", err)
		}
	}}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/admin/plans/"+f.plan.ID, strings.NewReader(`{"self_signup":true}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: f.plan.ID}}
	h.UpdatePlan(c)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "self_signup") {
		t.Fatalf("racing edit: %d %s", w.Code, w.Body.String())
	}
	var on bool
	if err := f.s.DB.NewRaw("SELECT self_signup FROM plans WHERE id = ?", f.plan.ID).Scan(f.ctx, &on); err != nil || on {
		t.Fatalf("self_signup = %v (%v), want false", on, err)
	}
}

// Whether the existing licence still works is judged with its plan's
// grace period, as verification judges it: a trial past its end on a
// plan with no grace is over, not covered by the default week.
func TestSelfSignup_ExistingUsableUsesThePlansGrace(t *testing.T) {
	f := newSignupFixture(t, func(p *model.Plan) { p.LicenseType = "trial"; p.TrialDays = 14 })
	email := "grace-" + f.plan.CheckoutID + "@example.com"
	if w, _ := f.verify(email, f.code(email), false); w.Code != http.StatusCreated {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	if _, err := f.s.DB.NewUpdate().Model((*model.Plan)(nil)).Set("grace_days = 0").
		Where("id = ?", f.plan.ID).Exec(f.ctx); err != nil {
		t.Fatalf("grace: %v", err)
	}
	if _, err := f.s.DB.NewUpdate().Model((*model.License)(nil)).Set("valid_until = now() - interval '1 hour'").
		Where("id = ?", f.licences(email)[0].ID).Exec(f.ctx); err != nil {
		t.Fatalf("expire: %v", err)
	}
	w, out := f.verify(email, f.code(email), false)
	if w.Code != http.StatusOK || dataField(out, "status") != "existing" || dataField(out, "license_usable") != false {
		t.Fatalf("claim on a trial past its end: %d %s", w.Code, w.Body.String())
	}
}
