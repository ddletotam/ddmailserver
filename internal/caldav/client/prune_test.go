package client

import (
	"testing"
	"time"

	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// The window SyncCalendar actually asks for: six months back, a year forward.
func syncWindow() (time.Time, time.Time) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	return now.AddDate(0, -6, 0), now.AddDate(1, 0, 0)
}

func msAt(y int, m time.Month, d int) int64 {
	return timeutil.ToMs(time.Date(y, m, d, 10, 0, 0, 0, time.UTC))
}

func TestKeepReasonDeletesEventInsideWindow(t *testing.T) {
	start, end := syncWindow()
	st := db.SyncPruneState{DTStart: msAt(2026, time.August, 20)}

	if reason := keepReason(st, start, end); reason != "" {
		t.Errorf("keepReason = %q, want empty — an in-window one-off absent upstream is deleted", reason)
	}
}

// The mechanism that ate the past: an event that aged past the boundary was
// never asked for, so its absence meant nothing, and it was deleted anyway.
func TestKeepReasonKeepsEventOlderThanWindow(t *testing.T) {
	start, end := syncWindow()
	st := db.SyncPruneState{DTStart: msAt(2026, time.January, 15)} // ~8 months back

	if reason := keepReason(st, start, end); reason == "" {
		t.Error("an event older than the window was pruned; it is absent only because nobody asked for it")
	}
}

func TestKeepReasonKeepsEventBeyondWindow(t *testing.T) {
	start, end := syncWindow()
	st := db.SyncPruneState{DTStart: msAt(2028, time.March, 1)} // past +1y

	if reason := keepReason(st, start, end); reason == "" {
		t.Error("an event further out than the window was pruned")
	}
}

// Birthdays: DTSTART in 2017, instances every year, so the remote returns them
// for a 2026 window. Absence cannot be trusted either way.
func TestKeepReasonKeepsRecurring(t *testing.T) {
	start, end := syncWindow()
	st := db.SyncPruneState{DTStart: msAt(2017, time.April, 30), Recurs: true}

	if reason := keepReason(st, start, end); reason == "" {
		t.Error("a recurring event was pruned on the strength of a windowed listing")
	}
}

func TestKeepReasonKeepsLocallyModified(t *testing.T) {
	start, end := syncWindow()
	st := db.SyncPruneState{DTStart: msAt(2026, time.September, 9), LocalModified: true}

	if reason := keepReason(st, start, end); reason == "" {
		t.Error("a locally modified event was pruned before it could be pushed")
	}
}

func TestKeepReasonKeepsQueuedForReverseSync(t *testing.T) {
	start, end := syncWindow()
	st := db.SyncPruneState{DTStart: msAt(2026, time.September, 9), PendingSync: true}

	if reason := keepReason(st, start, end); reason == "" {
		t.Error("an event with a pending reverse-sync operation was pruned")
	}
}

// Task collections are fetched without a time filter, so there the listing is
// the whole truth and an absent task really is gone.
func TestKeepReasonUnfilteredListingPrunes(t *testing.T) {
	st := db.SyncPruneState{DTStart: 0} // a task with no dates at all

	if reason := keepReason(st, time.Time{}, time.Time{}); reason != "" {
		t.Errorf("keepReason = %q, want empty — an unfiltered listing is authoritative", reason)
	}
}

func TestKeepReasonUnfilteredStillProtectsLocalWork(t *testing.T) {
	st := db.SyncPruneState{LocalModified: true}

	if reason := keepReason(st, time.Time{}, time.Time{}); reason == "" {
		t.Error("local work must survive even an authoritative listing")
	}
}

// Boundaries are inclusive: an event exactly on the edge was asked for.
func TestKeepReasonWindowEdgesAreInclusive(t *testing.T) {
	start, end := syncWindow()

	for name, ms := range map[string]int64{
		"start edge": timeutil.ToMs(start),
		"end edge":   timeutil.ToMs(end),
	} {
		if reason := keepReason(db.SyncPruneState{DTStart: ms}, start, end); reason != "" {
			t.Errorf("%s: keepReason = %q, want empty", name, reason)
		}
	}
}
