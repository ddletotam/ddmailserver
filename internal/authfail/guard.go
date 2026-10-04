package authfail

import (
	"fmt"
	"log"
	"time"

	"github.com/ddletotam/ddmailserver/internal/models"
)

// Store persists the per-subject state (implemented by *db.DB).
type Store interface {
	// AcquireAuthAttempt decides whether an attempt may be made now. With no
	// state it allows without writing anything. With an expired pause it
	// allows and moves next_attempt_at forward (RetryLease) in the same
	// transaction, so of several paths waking up at once only one logs in.
	AcquireAuthAttempt(kind string, id int64) (bool, *models.AuthBackoff, error)
	// RecordAuthFailure applies AfterFailure and returns the new state;
	// counted is false when the rejection fell into the burst window of the
	// previous one and did not count as a new failure.
	RecordAuthFailure(kind string, id, userID int64, errMsg string) (st *models.AuthBackoff, counted bool, err error)
	// ClearAuthBackoff deletes the state and returns what it was (nil when
	// there was none).
	ClearAuthBackoff(kind string, id int64) (*models.AuthBackoff, error)
}

// Subject is one set of credentials.
type Subject struct {
	Kind   string // models.AuthSubject*
	ID     int64
	UserID int64
	Label  string // for log lines: address or source name
}

// AccountIMAP is the IMAP login of an external account.
func AccountIMAP(a *models.Account) Subject {
	return Subject{Kind: models.AuthSubjectIMAP, ID: a.ID, UserID: a.UserID, Label: a.Email + " (IMAP)"}
}

// AccountSMTP is the SMTP login of an external account.
func AccountSMTP(a *models.Account) Subject {
	return Subject{Kind: models.AuthSubjectSMTP, ID: a.ID, UserID: a.UserID, Label: a.Email + " (SMTP)"}
}

// CalendarSource is the CalDAV login of a calendar source.
func CalendarSource(s *models.CalendarSource) Subject {
	return Subject{Kind: models.AuthSubjectCalDAV, ID: s.ID, UserID: s.UserID, Label: s.Name + " (CalDAV)"}
}

// ContactSource is the CardDAV login of a contact source.
func ContactSource(s *models.ContactSource) Subject {
	return Subject{Kind: models.AuthSubjectCardDAV, ID: s.ID, UserID: s.UserID, Label: s.Name + " (CardDAV)"}
}

// Guard is what a login path talks to: ask before the attempt, report after.
//
// Log lines are rationed on purpose: one when the credentials are first
// rejected, one per (rare) repeated rejection, one when they work again.
// A pause that suppressed an attempt says nothing — that is the normal state
// and used to be exactly the per-minute noise this replaces.
//
// A store error fails open: attempts proceed as they did before this existed.
// If the database is down, the login pause is the least of the problems.
type Guard struct {
	store Store
	logf  func(format string, args ...interface{})
}

// NewGuard returns a guard over store. A nil store gives a guard that allows
// everything and records nothing.
func NewGuard(store Store) *Guard {
	return &Guard{store: store, logf: log.Printf}
}

// Allow reports whether a login attempt for s may be made now.
func (g *Guard) Allow(s Subject) bool {
	if g == nil || g.store == nil {
		return true
	}
	ok, _, err := g.store.AcquireAuthAttempt(s.Kind, s.ID)
	if err != nil {
		g.logf("auth backoff [%s]: %v — attempting anyway", s.Label, err)
		return true
	}
	return ok
}

// Report records the outcome of a login attempt and reports whether it was a
// credentials rejection. nil means the credentials were accepted; an error
// that is neither (network, timeout) changes nothing.
//
// Report the login step, not the whole job: a sync that logged in and then
// failed on a folder still proves the password works.
func (g *Guard) Report(s Subject, err error) bool {
	if err == nil {
		g.Accepted(s)
		return false
	}
	if !Is(err) {
		return false
	}
	g.Rejected(s, err)
	return true
}

// Rejected records a credentials rejection and returns the new state.
func (g *Guard) Rejected(s Subject, cause error) *models.AuthBackoff {
	if g == nil || g.store == nil {
		return nil
	}
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	st, counted, err := g.store.RecordAuthFailure(s.Kind, s.ID, s.UserID, msg)
	if err != nil {
		g.logf("auth backoff [%s]: failed to record rejection: %v", s.Label, err)
		return nil
	}
	next := fmtMs(st.NextAttemptAt)
	switch {
	case !counted:
		// Another path of the same incident — already logged.
	case st.Failures == 1:
		g.logf("auth backoff [%s]: credentials rejected by the provider (%s) — no more login attempts until %s; update the password", s.Label, msg, next)
	default:
		g.logf("auth backoff [%s]: still rejected (%d in a row since %s): %s — next attempt at %s",
			s.Label, st.Failures, fmtMs(st.FirstFailureAt), msg, next)
	}
	return st
}

// Accepted clears the state after a successful login.
func (g *Guard) Accepted(s Subject) {
	if g == nil || g.store == nil {
		return
	}
	prev, err := g.store.ClearAuthBackoff(s.Kind, s.ID)
	if err != nil {
		g.logf("auth backoff [%s]: failed to clear: %v", s.Label, err)
		return
	}
	if prev != nil {
		g.logf("auth backoff [%s]: credentials accepted again after %d rejection(s) since %s",
			s.Label, prev.Failures, fmtMs(prev.FirstFailureAt))
	}
}

func fmtMs(ms int64) string {
	if ms == 0 {
		return "—"
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04:05")
}

// Describe is a one-line human summary of a state, for error texts and logs.
func Describe(st *models.AuthBackoff) string {
	if st == nil {
		return ""
	}
	return fmt.Sprintf("credentials rejected by the provider since %s, next attempt at %s",
		fmtMs(st.FirstFailureAt), fmtMs(st.NextAttemptAt))
}
