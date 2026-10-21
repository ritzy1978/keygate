package payment

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// A free plan opened for self sign-up sends /pay to its sign-up form
// instead of answering that payment is not configured.
func TestCheckoutByPlan_SelfSignupGoesToTheForm(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	gin.SetMode(gin.TestMode)
	plan := seedPlan(t, s, ctx, "signupredir", "subscription")
	plan.SelfSignup = true
	if err := s.UpdatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	h := &StripeHandler{Store: s, BaseURL: "https://keygate.example"}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/pay/"+plan.CheckoutID, nil)
	c.Params = gin.Params{{Key: "checkout_id", Value: plan.CheckoutID}}
	h.CheckoutByPlan(c)
	if w.Code != http.StatusFound {
		t.Fatalf("status %d, want 302: %s", w.Code, w.Body.String())
	}
	if got, want := w.Header().Get("Location"), "https://keygate.example/signup/"+plan.CheckoutID; got != want {
		t.Fatalf("Location %q, want %q", got, want)
	}
}
