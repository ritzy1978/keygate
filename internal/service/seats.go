package service

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
)

type SeatService struct {
	store   *store.Store
	webhook *WebhookService
	email   *EmailService
	// baseURL is the public origin used to build seat-invite claim
	// links (e.g. https://license.example.com). Empty in tests; the
	// service then falls back to a relative path so the email still
	// reads sensibly without a leading host.
	baseURL string
	logger  *slog.Logger
}

func NewSeatService(s *store.Store, wh *WebhookService, em *EmailService, logger *slog.Logger, baseURL string) *SeatService {
	return &SeatService{store: s, webhook: wh, email: em, logger: logger, baseURL: strings.TrimRight(baseURL, "/")}
}

type AddSeatInput struct {
	LicenseKey string
	Email      string
	Role       string
	ProductID  string
	// ActorUserID identifies the portal user driving the action,
	// for audit. Empty when invoked from an unauthenticated path
	// (none today; kept optional so legacy callers compile).
	ActorUserID string
	// ActorEmail is the signed-in address driving the action: only the
	// license owner manages the team (see requireOwner).
	ActorEmail string
}

// requireOwner holds team management to the licence's owner. Anyone
// else, members included, gets the same 404 an unknown licence does;
// the portal guard has turned them away already, this holds the line
// on its own.
func requireOwner(lic *model.License, email string) error {
	if email != "" && strings.EqualFold(lic.Email, email) {
		return nil
	}
	return licenseNotFound()
}

func (s *SeatService) AddSeat(ctx context.Context, in AddSeatInput) (*model.Seat, error) {
	// Any "can't act on this license right now" condition (missing
	// key, wrong product, non-seat product type, suspended/revoked/
	// expired) folds to one 404 so the endpoint can't probe license
	// existence or state. Who asks is settled before anything about
	// the request is: only the owner invites.
	lic, err := loadLicenseForSDK(ctx, s.store, in.LicenseKey, in.ProductID, model.CapSeats, true)
	if err != nil {
		return nil, err
	}
	if err := requireOwner(lic, in.ActorEmail); err != nil {
		return nil, err
	}

	// Normalize first so validation, self-invite check, DB write, and
	// later user-row lookups all agree on the same canonical form. PG
	// is case-sensitive on email; without this, "Foo@x.com" and
	// "foo@x.com" become two separate identities — re-invite hits the
	// wrong row, accept creates a duplicate user, the audit gets ugly.
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if appErr := apperr.ValidateEmail(in.Email); appErr != nil {
		return nil, appErr
	}
	// A team is its owner, who manages it, and members, who use the
	// license. The owner, identified by `license.email`, holds no seat
	// row; everyone invited joins as a member. Teams used to have
	// admins as well, a role nobody is given any more.
	if in.Role == "" {
		in.Role = "member"
	}
	if in.Role != "member" {
		return nil, apperr.BadRequest("role must be member")
	}

	// Self-invite guard. The license owner already has access via
	// the license; spinning up a seat for their own email creates a
	// confusing dual-binding (seat.user_id and license.email both
	// pointing at the same person). Cheap to block here, expensive
	// to untangle later. EqualFold catches "OWNER@x.com" even when
	// the license row stored the email in mixed case.
	if strings.EqualFold(in.Email, lic.Email) {
		return nil, apperr.BadRequest("cannot invite the license owner — they already have access")
	}

	// The plan's seats, or on a plan sold per seat the seats bought.
	maxSeats, _ := lic.Limits()

	// Mint the invite token BEFORE we hand the seat to the store.
	// The plain token only leaves this function inside the email
	// body and (via the SeatCreated/Restored path) the response —
	// it never enters the DB, only its SHA256 hash does.
	plainToken, tokenHash := store.GenerateSeatInviteToken()
	if plainToken == "" {
		return nil, apperr.Internal(errors.New("invite token generation failed"))
	}
	expiresAt := time.Now().Add(store.SeatInviteTTL)
	seat := &model.Seat{
		LicenseID:       lic.ID,
		Email:           in.Email,
		Role:            in.Role,
		InviteTokenHash: tokenHash,
		InviteExpiresAt: &expiresAt,
	}
	// Atomic count-and-insert under a per-license row lock. Without
	// this, two concurrent adds can both pass the count check and
	// both insert, overshooting max_seats. Also handles the
	// restore-on-reinvite case (removed_at IS NOT NULL → un-remove).
	outcome, staleDevices, err := s.store.AddSeatWithinLimit(ctx, seat, maxSeats)
	if err != nil {
		if errors.Is(err, store.ErrUpgradeCheckoutOpen) {
			return nil, apperr.Conflict("UPGRADE_IN_PROGRESS",
				"an upgrade of this license is waiting for payment; finish or close it before inviting people")
		}
		if errors.Is(err, store.ErrSeatLimitReached) {
			// People on the license, the owner included, as max_seats
			// counts them.
			members, _ := s.store.CountActiveSeats(ctx, lic.ID)
			return nil, apperr.WithDetails(
				apperr.Conflict("SEAT_LIMIT", "maximum seats reached"),
				map[string]any{"max": maxSeats, "current": members + 1},
			)
		}
		return nil, apperr.Internal(err)
	}

	// SeatExisted → idempotent no-op, suppress webhook + invite so a
	// retry doesn't double-fire. The plain token we just minted is
	// discarded — the existing live invite is what's valid.
	// SeatCreated / SeatRestored both represent a fresh seat-active
	// event; fire the webhook + send the email with the claim link.
	// SeatResent sends the email with the new link but adds no seat, so
	// no seat.added.
	if outcome != store.SeatExisted {
		if outcome != store.SeatResent {
			s.webhook.Dispatch(ctx, lic.ProductID, model.EventSeatAdded, map[string]any{
				"license_id": lic.ID, "seat_id": seat.ID, "email": in.Email,
				"role": in.Role, "outcome": string(outcome),
			})
		}
		// Devices a seat brought back still had, now gone with its old key.
		for _, a := range staleDevices {
			s.webhook.Dispatch(ctx, lic.ProductID, "license.deactivated", map[string]any{
				"license_id": lic.ID, "identifier": a.Identifier,
			})
		}
		if s.email != nil {
			productName, downloadURL := "", ""
			if lic.Product != nil {
				productName, downloadURL = lic.Product.Name, lic.Product.DownloadURL
			}
			s.email.SendSeatInvite(in.Email, productName, lic.Email, s.inviteURL(plainToken), downloadURL)
		}
		s.logger.Info("seat added", "license_id", lic.ID, "email", in.Email, "outcome", outcome)
	}
	// Audit every accepted call (including SeatExisted no-ops). The
	// audit row is the only durable record of an actor attempting
	// a seat change on a customer org — webhooks fire only on real
	// changes, but incident response wants to see attempts too.
	s.store.Audit(ctx, &model.AuditLog{
		Entity: "seat", EntityID: seat.ID, Action: "seat_invited",
		ActorType: "portal_user", ActorID: in.ActorUserID,
		Changes: map[string]any{
			"license_id": lic.ID, "email": in.Email, "role": in.Role,
			"outcome": string(outcome),
		},
	})

	return seat, nil
}

// inviteURL builds the absolute claim URL for the invite email. We
// keep the path stable (/accept-invite?token=...) so the frontend
// route is a fixed contract — the email body never embeds a deep
// link to internal admin paths.
func (s *SeatService) inviteURL(plainToken string) string {
	if s.baseURL == "" {
		return "/accept-invite?token=" + plainToken
	}
	return s.baseURL + "/accept-invite?token=" + plainToken
}

// AcceptSeatInviteResult is what POST /invites/accept returns. We hand back
// enough context for the client to redirect into the portal with a
// pre-selected license — the seat row itself stays internal.
type AcceptSeatInviteResult struct {
	UserID      string `json:"user_id"`
	Email       string `json:"email"`
	LicenseID   string `json:"license_id"`
	ProductName string `json:"product_name,omitempty"`
	Role        string `json:"role"`
}

// AcceptSeatInvite is the portal-facing entry point for claiming a
// seat invitation. The plain token IS the email-ownership proof, so
// we don't require a prior session. We auto-create a user record
// (keyed off the seat's email, NOT client input) if none exists.
//
// Failure cases (bad token, expired, removed, already accepted)
// all collapse to a single SEAT_INVITE_INVALID error so an attacker
// can't probe token validity by response shape.
func (s *SeatService) AcceptSeatInvite(ctx context.Context, plainToken string) (*AcceptSeatInviteResult, error) {
	user, seat, err := s.store.AcceptSeatInvite(ctx, plainToken)
	if err != nil {
		if errors.Is(err, store.ErrSeatInviteInvalid) {
			return nil, apperr.New(404, "SEAT_INVITE_INVALID",
				"this invitation is invalid or has expired")
		}
		// The token holder is the invitee, so saying why is no leak;
		// the invite stands and works once a seat is free.
		if errors.Is(err, store.ErrSeatLimitReached) {
			return nil, apperr.Conflict("SEAT_LIMIT",
				"no seat is free on this license right now; ask the license owner to free one, then open the invitation again")
		}
		return nil, apperr.Internal(err)
	}
	// Refuse to claim a seat on a license that's no longer in good
	// standing, by the rule an invite is held to (LicenseUsable): a
	// canceled license still inside its paid period takes the members it
	// invited. We undo the user_id + accepted_at fields so the row
	// is back to "invited" state and the owner can re-invite cleanly,
	// but the original token is deliberately *not* restored (see
	// store.RollbackSeatAccept) — the plaintext has already been
	// processed once, so re-issuing it would be a replay risk.
	// Folded into the same opaque error so an attacker probing
	// tokens can't tell license-state from invite-state.
	lic, lerr := s.store.FindLicenseByID(ctx, seat.LicenseID)
	if lerr != nil {
		return nil, apperr.Internal(lerr)
	}
	if LicenseUsable(lic) != nil {
		devices, rbErr := s.store.RollbackSeatAccept(ctx, seat.ID)
		if rbErr != nil {
			s.logger.Error("seat accept rollback failed", "seat_id", seat.ID, "err", rbErr)
		}
		for _, a := range devices {
			s.webhook.Dispatch(ctx, lic.ProductID, "license.deactivated", map[string]any{
				"license_id": lic.ID, "identifier": a.Identifier,
			})
		}
		return nil, apperr.New(404, "SEAT_INVITE_INVALID",
			"this invitation is invalid or has expired")
	}
	productName := ""
	if lic.Product != nil {
		productName = lic.Product.Name
	}
	return &AcceptSeatInviteResult{
		UserID:      user.ID,
		Email:       user.Email,
		LicenseID:   seat.LicenseID,
		ProductName: productName,
		Role:        seat.Role,
	}, nil
}

// RemoveSeat takes a member off the licence; only its owner does. Like
// releasing a device it works whatever the licence's state: an owner
// whose licence has lapsed or is suspended still takes access away, so
// a renewal does not hand it back. Removing a member already gone does
// nothing, and tells nobody twice.
func (s *SeatService) RemoveSeat(ctx context.Context, licenseKey, seatID, productID, actorUserID, actorEmail string) error {
	lic, err := loadLicenseForSDK(ctx, s.store, licenseKey, productID, model.CapSeats, false)
	if err != nil {
		return err
	}
	if err := requireOwner(lic, actorEmail); err != nil {
		return err
	}
	return s.removeSeat(ctx, lic, seatID, "portal_user", actorUserID, "")
}

// AdminRemoveSeat is RemoveSeat for the vendor's admin, who finds the
// licence by its ID: support takes a member off, or cancels an
// invitation, as the owner would. It works whatever the licence's state
// and its product's type, so a seat can always be cleared.
func (s *SeatService) AdminRemoveSeat(ctx context.Context, licenseID, seatID, adminID, ip string) error {
	lic, err := s.store.FindLicenseByID(ctx, licenseID)
	if errors.Is(err, sql.ErrNoRows) {
		return apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
	}
	if err != nil {
		return apperr.Internal(err)
	}
	return s.removeSeat(ctx, lic, seatID, "admin", adminID, ip)
}

// removeSeat takes a seat off the licence: the member's key stops
// working and their devices and floating sessions go with it, each
// device reported as deactivated. Removing a seat already removed is a
// quiet no-op, so a retry fires nothing twice.
func (s *SeatService) removeSeat(ctx context.Context, lic *model.License, seatID, actorType, actorID, ip string) error {
	seat, err := s.store.FindSeatByID(ctx, seatID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && seat.LicenseID != lic.ID) {
		return apperr.NotFound("SEAT", seatID)
	}
	if err != nil {
		return apperr.Internal(err)
	}

	devices, removed, err := s.store.RemoveSeat(ctx, seatID)
	if err != nil {
		return apperr.Internal(err)
	}
	if !removed {
		return nil
	}

	s.webhook.Dispatch(ctx, lic.ProductID, model.EventSeatRemoved, map[string]any{
		"license_id": lic.ID, "seat_id": seatID, "email": seat.Email,
	})
	for _, a := range devices {
		s.webhook.Dispatch(ctx, lic.ProductID, "license.deactivated", map[string]any{
			"license_id": lic.ID, "identifier": a.Identifier,
		})
	}
	s.store.Audit(ctx, &model.AuditLog{
		Entity: "seat", EntityID: seatID, Action: "seat_removed",
		ActorType: actorType, ActorID: actorID, IPAddress: ip,
		Changes: map[string]any{"license_id": lic.ID, "email": seat.Email, "devices_removed": len(devices)},
	})
	s.logger.Info("seat removed", "license_id", lic.ID, "seat_id", seatID, "devices_removed", len(devices), "by", actorType)

	return nil
}

// ListSeats is the team, whatever the licence's state, so an owner can
// see whom to remove while it is lapsed or suspended. The owner
// (actorEmail is license.email) sees pending invitations too; a member
// sees who has joined.
func (s *SeatService) ListSeats(ctx context.Context, licenseKey, productID, actorEmail string) ([]*model.Seat, error) {
	lic, err := loadLicenseForSDK(ctx, s.store, licenseKey, productID, model.CapSeats, false)
	if err != nil {
		return nil, err
	}

	seats, _, err := s.store.ListSeats(ctx, lic.ID, store.All)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	if !strings.EqualFold(lic.Email, actorEmail) {
		seats = model.JoinedSeats(seats)
	}
	if err := s.store.MarkPausedSeats(ctx, lic.ID, seats); err != nil {
		return nil, apperr.Internal(err)
	}
	return seats, nil
}
