package authfail

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/models"
)

// memStore is authfail.Store in memory, with a settable clock. It mirrors the
// DB implementation's decisions (same AfterFailure / RetryLease).
type memStore struct {
	mu    sync.Mutex
	now   int64
	rows  map[string]*models.AuthBackoff
	calls int
}

func newMemStore() *memStore {
	return &memStore{now: 1_700_000_000_000, rows: map[string]*models.AuthBackoff{}}
}

func key(kind string, id int64) string { return fmt.Sprintf("%s/%d", kind, id) }

func (m *memStore) AcquireAuthAttempt(kind string, id int64) (bool, *models.AuthBackoff, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	st := m.rows[key(kind, id)]
	if st == nil {
		return true, nil, nil
	}
	if st.Paused(m.now) {
		return false, st, nil
	}
	st.NextAttemptAt = RetryLease(st, m.now)
	return true, st, nil
}

func (m *memStore) RecordAuthFailure(kind string, id, userID int64, msg string) (*models.AuthBackoff, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.rows[key(kind, id)]
	st := AfterFailure(prev, kind, id, userID, m.now, msg)
	counted := prev == nil || st.Failures != prev.Failures
	m.rows[key(kind, id)] = st
	return st, counted, nil
}

func (m *memStore) ClearAuthBackoff(kind string, id int64) (*models.AuthBackoff, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.rows[key(kind, id)]
	delete(m.rows, key(kind, id))
	return st, nil
}

func (m *memStore) advance(ms int64) {
	m.mu.Lock()
	m.now += ms
	m.mu.Unlock()
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...interface{}) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func newTestGuard(s Store) (*Guard, *logSink) {
	sink := &logSink{}
	return &Guard{store: s, logf: sink.logf}, sink
}

var acct = Subject{Kind: models.AuthSubjectIMAP, ID: 42, UserID: 1, Label: "user@yandex.ru (IMAP)"}

// TestGuard_RevokedPassword is the production incident replayed with a clock:
// the scheduler offers a login every minute for two hours. Before, each offer
// was a failed LOGIN — 120 of them. Now the provider sees one at +0, +1, +6,
// +21 min, and then hourly.
func TestGuard_RevokedPassword(t *testing.T) {
	store := newMemStore()
	g, logs := newTestGuard(store)
	rejection := errors.New("failed to login: " + Mark(errors.New("LOGIN invalid credentials or IMAP is disabled")).Error())
	rejection = Mark(rejection)

	var attemptsAt []int64
	start := store.now
	for minute := 0; minute < 120; minute++ {
		if g.Allow(acct) {
			attemptsAt = append(attemptsAt, (store.now-start)/60000)
			if !g.Report(acct, rejection) {
				t.Fatal("rejection not reported as such")
			}
		}
		store.advance(60_000)
	}
	want := []int64{0, 1, 6, 21, 81}
	if fmt.Sprint(attemptsAt) != fmt.Sprint(want) {
		t.Fatalf("login attempts at minutes %v, want %v", attemptsAt, want)
	}
	// One line for the transition, one per repeated rejection — nothing for
	// the 115 suppressed offers.
	if len(logs.lines) != len(want) {
		t.Fatalf("%d log lines, want %d:\n%s", len(logs.lines), len(want), strings.Join(logs.lines, "\n"))
	}
	if !strings.Contains(logs.lines[0], "credentials rejected by the provider") {
		t.Errorf("first line: %q", logs.lines[0])
	}
	if !strings.Contains(logs.lines[1], "still rejected (2 in a row") {
		t.Errorf("second line: %q", logs.lines[1])
	}
}

// TestGuard_NetworkErrorChangesNothing: a timeout is not a verdict on the
// password — no pause starts, and an existing one is not lifted.
func TestGuard_NetworkErrorChangesNothing(t *testing.T) {
	store := newMemStore()
	g, _ := newTestGuard(store)
	if g.Report(acct, errors.New("dial tcp: i/o timeout")) {
		t.Fatal("timeout reported as rejection")
	}
	if len(store.rows) != 0 {
		t.Fatal("timeout started a pause")
	}
	g.Report(acct, Mark(errors.New("bad password")))
	g.Report(acct, errors.New("connection reset"))
	if store.rows[key(acct.Kind, acct.ID)] == nil {
		t.Fatal("network error lifted the pause")
	}
}

// TestGuard_SuccessClears: a successful login ends the state, logs once, and
// the next failure is a fresh incident (one-minute pause, new notification).
func TestGuard_SuccessClears(t *testing.T) {
	store := newMemStore()
	g, logs := newTestGuard(store)
	for i := 0; i < 3; i++ {
		g.Rejected(acct, errors.New("nope"))
		store.advance(3_600_000)
	}
	g.Report(acct, nil)
	if len(store.rows) != 0 {
		t.Fatal("success did not clear")
	}
	if last := logs.lines[len(logs.lines)-1]; !strings.Contains(last, "accepted again after 3 rejection") {
		t.Fatalf("recovery line: %q", last)
	}
	n := len(logs.lines)
	g.Report(acct, nil) // no state, no line
	if len(logs.lines) != n {
		t.Fatal("success without a state logged")
	}
	st := g.Rejected(acct, errors.New("nope"))
	if st.Failures != 1 || st.NextAttemptAt-store.now != 60_000 {
		t.Fatalf("after recovery: %+v", st)
	}
}

// TestGuard_ResetAllowsAtOnce: editing the password deletes the state
// (db.UpdateAccount → resetAccountAuthBackoff); the very next offer logs in.
func TestGuard_ResetAllowsAtOnce(t *testing.T) {
	store := newMemStore()
	g, _ := newTestGuard(store)
	for i := 0; i < 5; i++ {
		g.Rejected(acct, errors.New("nope"))
		store.advance(31_000)
	}
	if g.Allow(acct) {
		t.Fatal("allowed during the pause")
	}
	store.ClearAuthBackoff(acct.Kind, acct.ID) // what the edit does
	if !g.Allow(acct) {
		t.Fatal("not allowed right after the reset")
	}
}

// TestGuard_OneAttemptPerWindow: when the pause ends, IDLE, the poll and flag
// sync all ask at once. One gets the login; the rest wait for its verdict.
func TestGuard_OneAttemptPerWindow(t *testing.T) {
	store := newMemStore()
	g, _ := newTestGuard(store)
	g.Rejected(acct, errors.New("nope"))
	store.advance(61_000)

	allowed := 0
	for i := 0; i < 4; i++ {
		if g.Allow(acct) {
			allowed++
		}
	}
	if allowed != 1 {
		t.Fatalf("%d paths allowed in one window, want 1", allowed)
	}
}

// TestGuard_SubjectsAreSeparate: a rejected SMTP password does not stop IMAP
// (and vice versa) — the two are different credentials.
func TestGuard_SubjectsAreSeparate(t *testing.T) {
	store := newMemStore()
	g, _ := newTestGuard(store)
	a := &models.Account{ID: 9, UserID: 1, Email: "x@example.org"}
	g.Rejected(AccountSMTP(a), errors.New("535"))
	if !g.Allow(AccountIMAP(a)) {
		t.Fatal("SMTP rejection paused IMAP")
	}
	if g.Allow(AccountSMTP(a)) {
		t.Fatal("SMTP not paused")
	}
}

// failingStore: the database is down.
type failingStore struct{}

func (failingStore) AcquireAuthAttempt(string, int64) (bool, *models.AuthBackoff, error) {
	return false, nil, errors.New("db down")
}
func (failingStore) RecordAuthFailure(string, int64, int64, string) (*models.AuthBackoff, bool, error) {
	return nil, false, errors.New("db down")
}
func (failingStore) ClearAuthBackoff(string, int64) (*models.AuthBackoff, error) {
	return nil, errors.New("db down")
}

func TestGuard_FailsOpen(t *testing.T) {
	g, _ := newTestGuard(failingStore{})
	if !g.Allow(acct) {
		t.Fatal("store error blocked the attempt")
	}
	if st := g.Rejected(acct, errors.New("x")); st != nil {
		t.Fatal("state from a failing store")
	}
	var nilGuard *Guard
	if !nilGuard.Allow(acct) {
		t.Fatal("nil guard blocks")
	}
	nilGuard.Accepted(acct)
	if NewGuard(nil).Rejected(acct, errors.New("x")) != nil {
		t.Fatal("guard without store recorded")
	}
}

// TestHTTPWatch_ReportTo: the DAV verdict reaches the guard with a readable
// reason, whatever the job's own error says.
func TestHTTPWatch_ReportTo(t *testing.T) {
	store := newMemStore()
	g, _ := newTestGuard(store)
	src := Subject{Kind: models.AuthSubjectCalDAV, ID: 5, UserID: 1, Label: "Yandex (CalDAV)"}

	w := &HTTPWatch{unauthorized: true}
	if !w.ReportTo(g, src, errors.New("partial sync errors: [sync Работа: REPORT returned 401]")) {
		t.Fatal("401 not reported")
	}
	st := store.rows[key(src.Kind, src.ID)]
	if st == nil || !strings.Contains(st.LastError, "401") {
		t.Fatalf("state: %+v", st)
	}

	ok := &HTTPWatch{ok: true}
	ok.ReportTo(g, src, nil)
	if len(store.rows) != 0 {
		t.Fatal("accepted session did not clear")
	}
}
