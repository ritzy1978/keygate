package service

import (
	"context"
	"database/sql"
	"errors"
)

// ToggleableEmails lists the automated customer emails an admin may turn
// off (issue #36), in the order the dashboard shows them. Each is stored
// as the setting "email_notify_<kind>"; only the value "false" turns it
// off, so an install that never touched the setting sends everything as
// before.
//
// Deliberately absent, because they always go out:
//   - login codes (OTP), admin and seat invites — without them nobody
//     can get in;
//   - the license delivery email (license_created) — it carries what the
//     customer just bought;
//   - anything an admin sends on purpose, such as "resend license email".
//
// Several of them have a matching webhook event (license.expired,
// license.suspended, license.canceled, plan.changed, the payment events,
// quota.warning), so an install that turns one off can send its own
// mail from that event instead. The reminders have no event.
var ToggleableEmails = []string{
	// Reminders
	"license_expiring",
	"renewal_reminder",
	"updates_ending",
	"trial_ending",
	// License status changes
	"license_expired",
	"trial_expired", // also a trial moved to its fallback plan
	"license_suspended",
	"subscription_canceled",
	"plan_changed",
	// Billing
	"payment_failed", // also covers the follow-up dunning reminders
	"payment_recovered",
	"payment_action_required",
	// Usage and account
	"quota_warning",
	"welcome",
}

// NotifySettingKey is the setting that holds kind's on/off switch.
func NotifySettingKey(kind string) string { return "email_notify_" + kind }

// NotifyEnabled reports whether the automated email of the given kind
// should be sent. A read failure answers true: dropping a customer's
// mail because the settings table hiccuped is the worse mistake.
func (s *EmailService) NotifyEnabled(kind string) bool {
	if s == nil || s.store == nil {
		return true
	}
	v, err := s.store.GetSetting(context.Background(), NotifySettingKey(kind))
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	if err != nil {
		s.logger.Warn("email notification setting unreadable; sending", "kind", kind, "error", err)
		return true
	}
	return v != "false"
}

// skipDisabled logs and reports true when kind is switched off.
func (s *EmailService) skipDisabled(kind, to string) bool {
	if s.NotifyEnabled(kind) {
		return false
	}
	s.logger.Info("email not sent: turned off in settings", "kind", kind, "to", to)
	return true
}
