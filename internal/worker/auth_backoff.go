package worker

import (
	"log"

	"github.com/ddletotam/ddmailserver/internal/authfail"
	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/notify"
)

// authGuard returns the login-pause guard over database (see package
// authfail). A nil database gives a guard that allows everything.
func authGuard(database *db.DB) *authfail.Guard {
	if database == nil {
		return authfail.NewGuard(nil)
	}
	return authfail.NewGuard(database)
}

// reportDAVAuth records the verdict on a DAV source's credentials after one
// job. What the server answered on the wire decides (watch); without a
// verdict there — the job failed before any request, e.g. the OAuth refresh
// came back invalid_grant — a marked job error still counts as a rejection.
// Returns true when the credentials were rejected.
func reportDAVAuth(g *authfail.Guard, s authfail.Subject, watch *authfail.HTTPWatch, jobErr error) bool {
	if watch != nil && (watch.Rejected() || watch.Accepted()) {
		return watch.ReportTo(g, s, jobErr)
	}
	if authfail.Is(jobErr) {
		g.Rejected(s, jobErr)
		return true
	}
	return false
}

// authPaused is the scheduler's view of the login pauses: which subjects of
// each kind may not be attempted right now. Loaded once per tick.
type authPaused map[string]map[int64]bool

func (s *Scheduler) loadAuthPaused() authPaused {
	p := make(authPaused, 4)
	for _, kind := range []string{models.AuthSubjectIMAP, models.AuthSubjectSMTP, models.AuthSubjectCalDAV, models.AuthSubjectCardDAV} {
		ids, err := s.database.GetAuthPausedIDs(kind)
		if err != nil {
			// Fail open: without the filter the tasks' own guard still
			// arbitrates, and the logs are the only cost.
			log.Printf("Failed to load auth pauses (%s): %v", kind, err)
			continue
		}
		p[kind] = ids
	}
	return p
}

func (p authPaused) has(kind string, id int64) bool {
	return p[kind][id]
}

// publishAuthFailures tells users, once per transition, that the provider
// stopped accepting the credentials of one of their accounts or sources: a
// WebSocket push (auth_failed) for the desktop client. Rows are flagged
// notified so the next tick does not repeat it; a new incident after a
// successful login starts unflagged again.
func (s *Scheduler) publishAuthFailures() {
	states, err := s.database.GetUnnotifiedAuthBackoffs()
	if err != nil {
		log.Printf("Failed to get unnotified auth failures: %v", err)
		return
	}
	for _, st := range states {
		if s.notifyHub != nil {
			identity, name := s.authSubjectIdentity(st)
			s.notifyHub.Publish(notify.Event{
				UserID:        st.UserID,
				Type:          notify.EventAuthFailed,
				Identity:      identity,
				SubjectName:   name,
				Service:       st.Service(),
				Since:         st.FirstFailureAt,
				NextAttemptAt: st.NextAttemptAt,
			})
		}
		if err := s.database.MarkAuthBackoffNotified(st.SubjectKind, st.SubjectID, st.FirstFailureAt); err != nil {
			log.Printf("Failed to mark auth failure notified (%s %d): %v", st.SubjectKind, st.SubjectID, err)
		}
	}
}

// authSubjectIdentity resolves the address and display name of a subject for
// the push. Best-effort: an unknown subject yields empty strings.
func (s *Scheduler) authSubjectIdentity(st *models.AuthBackoff) (identity, name string) {
	switch st.SubjectKind {
	case models.AuthSubjectIMAP, models.AuthSubjectSMTP:
		if a, err := s.database.GetAccountByID(st.SubjectID); err == nil {
			return a.Email, a.Name
		}
	case models.AuthSubjectCalDAV:
		if src, err := s.database.GetCalendarSourceByID(st.SubjectID); err == nil && src != nil {
			return src.IdentityEmail, src.Name
		}
	case models.AuthSubjectCardDAV:
		if src, err := s.database.GetContactSourceByID(st.SubjectID); err == nil && src != nil {
			return src.IdentityEmail, src.Name
		}
	}
	return "", ""
}
