package authfail

import (
	"strings"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/models"
)

func TestDelay(t *testing.T) {
	cases := []struct {
		failures   int
		sinceFirst time.Duration
		want       time.Duration
	}{
		{0, 0, 0},
		{1, 0, time.Minute},
		{2, time.Minute, 5 * time.Minute},
		{3, 6 * time.Minute, 15 * time.Minute},
		{4, 21 * time.Minute, time.Hour},
		{5, 81 * time.Minute, time.Hour},
		{20, 20 * time.Hour, time.Hour},
		// A day of rejections: down to four attempts a day.
		{25, 24 * time.Hour, 6 * time.Hour},
		{40, 5 * 24 * time.Hour, 6 * time.Hour},
		// The slow cadence needs both the day and the escalation; a
		// state younger than a day stays hourly.
		{4, 25 * time.Hour, time.Hour},
	}
	for _, c := range cases {
		if got := Delay(c.failures, c.sinceFirst); got != c.want {
			t.Errorf("Delay(%d, %v) = %v, want %v", c.failures, c.sinceFirst, got, c.want)
		}
	}
}

// TestAfterFailure_Schedule walks a revoked password through a day: the
// attempts land at +1, +5, +15 min and then hourly, then every 6 hours.
func TestAfterFailure_Schedule(t *testing.T) {
	const min = int64(60 * 1000)
	now := int64(1_700_000_000_000)

	st := AfterFailure(nil, models.AuthSubjectIMAP, 7, 3, now, "LOGIN invalid credentials")
	if st.Failures != 1 || st.FirstFailureAt != now || st.NextAttemptAt != now+1*min {
		t.Fatalf("first: %+v", st)
	}
	if st.SubjectKind != models.AuthSubjectIMAP || st.SubjectID != 7 || st.UserID != 3 {
		t.Fatalf("identity lost: %+v", st)
	}

	wantGaps := []int64{5, 15, 60, 60, 60}
	for i, gap := range wantGaps {
		now = st.NextAttemptAt
		st = AfterFailure(st, models.AuthSubjectIMAP, 7, 3, now, "again")
		if st.Failures != i+2 {
			t.Fatalf("step %d: failures %d", i, st.Failures)
		}
		if got := (st.NextAttemptAt - now) / min; got != gap {
			t.Fatalf("step %d: pause %d min, want %d", i, got, gap)
		}
	}

	// Jump past a day of hourly attempts.
	now = st.FirstFailureAt + 24*60*min
	st = AfterFailure(st, models.AuthSubjectIMAP, 7, 3, now, "still")
	if got := (st.NextAttemptAt - now) / min; got != 6*60 {
		t.Fatalf("after a day: pause %d min, want 360", got)
	}
}

// TestAfterFailure_BurstIsOneIncident: when a password is revoked, sync, IDLE
// and flag sync all fail within seconds. That is one rejection, not three —
// otherwise the first pause would be 15 minutes instead of one.
func TestAfterFailure_BurstIsOneIncident(t *testing.T) {
	now := int64(1_700_000_000_000)
	st := AfterFailure(nil, models.AuthSubjectIMAP, 1, 1, now, "a")
	st2 := AfterFailure(st, models.AuthSubjectIMAP, 1, 1, now+2000, "b")
	if st2.Failures != 1 || st2.NextAttemptAt != st.NextAttemptAt {
		t.Fatalf("burst counted: %+v", st2)
	}
	if st2.LastError != "b" || st2.LastFailureAt != now+2000 {
		t.Fatalf("burst should still refresh the error: %+v", st2)
	}
	// Past the window it counts.
	st3 := AfterFailure(st2, models.AuthSubjectIMAP, 1, 1, now+61_000, "c")
	if st3.Failures != 2 {
		t.Fatalf("retry after the pause not counted: %+v", st3)
	}
}

func TestAfterFailure_TruncatesErrorOnRuneBoundary(t *testing.T) {
	long := strings.Repeat("пароль", 200)
	st := AfterFailure(nil, models.AuthSubjectIMAP, 1, 1, 1, long)
	if n := len([]rune(st.LastError)); n != maxErrorLen {
		t.Fatalf("len = %d runes", n)
	}
	if !strings.HasPrefix(long, st.LastError) {
		t.Fatal("truncation broke the text")
	}
}

func TestRetryLease(t *testing.T) {
	const min = int64(60 * 1000)
	st := &models.AuthBackoff{Failures: 3, FirstFailureAt: 0}
	if got := RetryLease(st, 30*min) - 30*min; got != 15*min {
		t.Fatalf("lease %d min, want 15", got/min)
	}
}
