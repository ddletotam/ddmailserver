package authfail

import (
	"time"

	"github.com/ddletotam/ddmailserver/internal/models"
)

// Pause schedule after consecutive rejections: 1 → 5 → 15 → 60 minutes, then
// hourly; once the credentials have been failing for a day — 6 hours.
//
// Why these numbers. Providers lock an account after roughly ten failed logins
// in a short window (minutes to an hour); the first three steps spend three
// attempts in the first 21 minutes — enough to ride out a one-off glitch on
// the provider side (Yandex has answered "invalid credentials" during its own
// outages) and far from any lockout threshold. Hourly after that is 24 failed
// logins a day: invisible to rate-based protection, and the user who fixes the
// password does not wait for it — editing the credentials resets the pause.
// After a day of rejections nobody is coming back soon; 6 hours keeps the
// account's security log quiet (4 entries a day) while still noticing a
// password that was fixed on the provider's side (an app password re-enabled).
var steps = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	60 * time.Minute,
}

const (
	// longRejection: rejected this long → the slow cadence.
	longRejection = 24 * time.Hour
	slowStep      = 6 * time.Hour

	// burstWindow: rejections closer together than this are one incident.
	// When the password is revoked, every path that logs in with it (sync,
	// IDLE, flag sync…) fails within seconds of each other; counting each as
	// a separate failure would jump straight to the hour-long pause.
	burstWindow = 30 * time.Second

	maxErrorLen = 500
)

// Delay is the pause after the failures-th rejection in a row, when the first
// one was sinceFirst ago.
func Delay(failures int, sinceFirst time.Duration) time.Duration {
	if failures <= 0 {
		return 0
	}
	if sinceFirst >= longRejection && failures > len(steps) {
		return slowStep
	}
	if failures > len(steps) {
		return steps[len(steps)-1]
	}
	return steps[failures-1]
}

// AfterFailure is the state after one more rejection at nowMs. prev is the
// current state (nil when the credentials were fine until now).
func AfterFailure(prev *models.AuthBackoff, kind string, id, userID, nowMs int64, errMsg string) *models.AuthBackoff {
	if r := []rune(errMsg); len(r) > maxErrorLen {
		errMsg = string(r[:maxErrorLen])
	}
	if prev == nil || prev.Failures <= 0 {
		return &models.AuthBackoff{
			SubjectKind:    kind,
			SubjectID:      id,
			UserID:         userID,
			Failures:       1,
			FirstFailureAt: nowMs,
			LastFailureAt:  nowMs,
			NextAttemptAt:  nowMs + Delay(1, 0).Milliseconds(),
			LastError:      errMsg,
		}
	}
	next := *prev
	next.LastError = errMsg
	if nowMs-prev.LastFailureAt < burstWindow.Milliseconds() {
		// Same incident as the previous rejection: keep the counter and the
		// pause it already earned.
		next.LastFailureAt = nowMs
		return &next
	}
	next.Failures = prev.Failures + 1
	next.LastFailureAt = nowMs
	since := time.Duration(nowMs-prev.FirstFailureAt) * time.Millisecond
	next.NextAttemptAt = nowMs + Delay(next.Failures, since).Milliseconds()
	return &next
}

// RetryLease is how far an attempt allowed after a pause pushes the next one
// out — the current step again. If the attempt ends without a verdict (the
// network failed before the server could judge the password) it still counts
// as a used window; a rejection replaces it with the next step.
func RetryLease(st *models.AuthBackoff, nowMs int64) int64 {
	since := time.Duration(nowMs-st.FirstFailureAt) * time.Millisecond
	return nowMs + Delay(st.Failures, since).Milliseconds()
}
