package models

// Subject kinds of auth_backoff (migrations/052_auth_backoff.sql). Each one
// is a separate set of credentials with its own pause.
const (
	AuthSubjectIMAP    = "account_imap"    // accounts.id, IMAP login
	AuthSubjectSMTP    = "account_smtp"    // accounts.id, SMTP AUTH for the outbox
	AuthSubjectCalDAV  = "calendar_source" // calendar_sources.id
	AuthSubjectCardDAV = "contact_source"  // contact_sources.id
)

// AuthBackoff is the "provider does not accept these credentials" state of one
// subject. It exists only while the credentials keep being rejected; a
// successful login or an edit of the credentials removes it.
//
// Times are unix milliseconds, like everywhere else in the schema.
type AuthBackoff struct {
	SubjectKind    string
	SubjectID      int64
	UserID         int64
	Failures       int
	FirstFailureAt int64
	LastFailureAt  int64
	NextAttemptAt  int64
	LastError      string
	NotifiedAt     int64
}

// AuthFailureView is what API clients get for a subject whose credentials are
// being rejected: which service, since when, when the next attempt is.
type AuthFailureView struct {
	Service       string `json:"service"` // imap, smtp, caldav, carddav
	Since         int64  `json:"since"`   // first rejection in this run, unix ms
	NextAttemptAt int64  `json:"next_attempt_at"`
	Failures      int    `json:"failures"`
	LastError     string `json:"last_error"`
}

// View converts the state into its API shape.
func (b *AuthBackoff) View() AuthFailureView {
	return AuthFailureView{
		Service:       b.Service(),
		Since:         b.FirstFailureAt,
		NextAttemptAt: b.NextAttemptAt,
		Failures:      b.Failures,
		LastError:     b.LastError,
	}
}

// Paused reports whether no attempt may be made at nowMs.
func (b *AuthBackoff) Paused(nowMs int64) bool {
	return b != nil && b.NextAttemptAt > nowMs
}

// Service is the short protocol name of the subject for UIs and API clients:
// imap, smtp, caldav or carddav.
func (b *AuthBackoff) Service() string {
	if b == nil {
		return ""
	}
	switch b.SubjectKind {
	case AuthSubjectIMAP:
		return "imap"
	case AuthSubjectSMTP:
		return "smtp"
	case AuthSubjectCalDAV:
		return "caldav"
	case AuthSubjectCardDAV:
		return "carddav"
	}
	return b.SubjectKind
}
