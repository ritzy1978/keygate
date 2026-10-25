package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

// PortalLicenseView shapes a licence for the signed-in address on the
// portal's licence list. Each person gets their own key: the owner the
// licence's, a member theirs, made the first time they get here (see
// Store.MemberKey). A member sees the devices their key activated and
// the members who have joined; activation_count stays the team's. ok is false when the address is
// neither the owner nor a member any more, as when the seat went in
// between the queries that read the list, or when the product has no
// seats now, so a member's key opens nothing: they get nothing, and
// never the owner's key. lic.Product, lic.Seats and lic.Activations
// must be loaded.
func PortalLicenseView(ctx context.Context, st *store.Store, lic *model.License, email string) (key string, ok bool, err error) {
	if strings.EqualFold(lic.Email, email) {
		key = st.DecryptLicenseKey(lic)
	} else {
		viewer := PortalViewerSeat(lic, email)
		if viewer == nil || lic.Product == nil || !model.ProductSupports(lic.Product.Type, model.CapSeats) {
			return "", false, nil
		}
		// Read the seat again: the list's copy may predate a removal,
		// and still hold the key the removal cleared.
		fresh, err := st.FindSeatByID(ctx, viewer.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", false, err
		}
		if err != nil || fresh.RemovedAt != nil || fresh.AcceptedAt == nil {
			return "", false, nil
		}
		if key, err = st.MemberKey(ctx, fresh); err != nil {
			return "", false, err
		}
		// Removed between that read and making the key: none was made.
		if key == "" {
			return "", false, nil
		}
		lic.Activations = OwnDevices(lic.Activations, viewer.ID)
		lic.Seats = model.JoinedSeats(lic.Seats)
		trimForMember(lic)
		// Over the seat limit, their key opens nothing usable: they are
		// told why, not left with a key that silently fails.
		if lic.KeySeatPaused, err = st.SeatPaused(ctx, fresh.ID); err != nil {
			return "", false, err
		}
	}
	if err := st.MarkPausedSeats(ctx, lic.ID, lic.Seats); err != nil {
		return "", false, err
	}
	if err := st.FillMemberEmails(ctx, lic.Activations); err != nil {
		return "", false, err
	}
	return key, true, nil
}

// trimForMember leaves out of a member's copy of the licence what is the
// owner's alone: the billing references, the admin's notes, the owner's
// marketing consent and sign-up details, and the accounts behind the
// other seats. The portal shows a member none of it; billing actions are
// the owner's.
func trimForMember(lic *model.License) {
	lic.UserID = ""
	lic.StripeCustomerID = ""
	lic.StripeSubscriptionID = ""
	lic.StripePaymentIntentID = ""
	lic.StripeCheckoutSessionID = ""
	lic.StripePendingUpdateExpiresAt = nil
	lic.Notes = ""
	lic.ExternalCustomerID = ""
	lic.ExternalWorkspaceID = ""
	lic.SignupSource = ""
	lic.SignupPlanID = ""
	lic.MarketingConsent = false
	lic.MarketingConsentAt = nil
	for _, seat := range lic.Seats {
		seat.UserID = ""
	}
}

// The portal guards read license_key from the request body. Without an
// explicit cap a logged-in user could stream gigabytes through the body
// buffer; session auth is a weak defense against compromised
// credentials sending memory-exhaustion payloads. 64 KiB is comfortably
// above any real portal request (license_key + a handful of fields).
const portalGuardMaxBody = 64 * 1024

// guardedLicense reads the request's license_key and the licence it
// opens, for the portal guards. It caps the body and restores an
// in-memory reader for the handler, so c.ShouldBindJSON still sees the
// same bytes. The response is written when ok is false, a lookup that
// failed for any reason but "no such licence" included; lic is nil, with
// ok true, when there is no key or no such licence, and the handler
// answers that.
func guardedLicense(c *gin.Context, st *store.Store) (lic *model.License, email string, ok bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, portalGuardMaxBody)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		response.Err(c, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE",
			"request body exceeds 64 KiB")
		c.Abort()
		return nil, "", false
	}
	c.Request.Body = io.NopCloser(bytes.NewBuffer(body))
	var req struct {
		LicenseKey string `json:"license_key"`
	}
	if json.Unmarshal(body, &req) != nil || req.LicenseKey == "" {
		return nil, "", true
	}
	// Defense in depth: a malformed JWT that produced an empty email
	// claim must not fall through to seat lookups with "".
	email = emailFromContext(c)
	if email == "" {
		response.Unauthorized(c, "unauthorized")
		c.Abort()
		return nil, "", false
	}
	lic, err = st.FindLicenseByKey(c.Request.Context(), req.LicenseKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, email, true
	}
	// Any other failure stops here: letting it through would skip the
	// check this guard exists for.
	if err != nil {
		response.Internal(c, err)
		c.Abort()
		return nil, "", false
	}
	return lic, email, true
}

// notYours turns the caller away with the same 404, code included, the
// handler returns when the licence does not exist: a differential
// between "exists but not yours" and "doesn't exist" would be a
// licence-existence oracle.
func notYours(c *gin.Context) {
	response.Err(c, http.StatusNotFound, "LICENSE_NOT_FOUND", "license not found")
	c.Abort()
}

// PortalLicenseGuard lets through the licence's owner and its accepted
// members; an invite not yet accepted gives no access. Emails compare
// case-insensitively: admin-created licences may have stored mixed
// case.
func PortalLicenseGuard(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		lic, email, ok := guardedLicense(c, st)
		if !ok {
			return
		}
		if lic != nil && !strings.EqualFold(lic.Email, email) {
			seat, err := st.FindSeatByEmail(c.Request.Context(), lic.ID, email)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				response.Internal(c, err)
				c.Abort()
				return
			}
			if err != nil || seat.AcceptedAt == nil {
				notYours(c)
				return
			}
		}
		c.Next()
	}
}

// PortalSeatOwnerGuard lets through the licence's owner alone: the
// owner manages the team, members use the licence.
func PortalSeatOwnerGuard(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		lic, email, ok := guardedLicense(c, st)
		if !ok {
			return
		}
		if lic != nil && !strings.EqualFold(lic.Email, email) {
			notYours(c)
			return
		}
		c.Next()
	}
}
