package worker

import (
	"testing"

	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
)

func row(id int64, uid, summary string, dtstart int64) db.EventIdentity {
	return db.EventIdentity{ID: id, UID: uid, ETag: `"etag-` + uid + `"`, Summary: summary, DTStart: dtstart}
}

func feedEvent(uid, summary string, dtstart int64) *models.CalendarEvent {
	return &models.CalendarEvent{UID: uid, Summary: summary, DTStart: dtstart, ETag: `"etag-` + uid + `"`}
}

// TestStableUIDsMatchOnUID: the ordinary case must be untouched by the content
// pass — a well-behaved feed is still matched on its UIDs alone.
func TestStableUIDsMatchOnUID(t *testing.T) {
	existing := []db.EventIdentity{
		row(1, "uid-a", "Standup", 1000),
		row(2, "uid-b", "Review", 2000),
	}
	parsed := []*models.CalendarEvent{
		feedEvent("uid-a", "Standup", 1000),
		feedEvent("uid-b", "Review", 2000),
	}

	matches, creates, deletes := matchFeedEvents(existing, parsed)

	if len(matches) != 2 || len(creates) != 0 || len(deletes) != 0 {
		t.Fatalf("got %d matches, %d creates, %d deletes; want 2/0/0", len(matches), len(creates), len(deletes))
	}
	for _, m := range matches {
		if m.ByContent {
			t.Errorf("event %s matched by content although its UID was stable", m.Event.UID)
		}
	}
}

// TestReissuedUIDsAreMatchedByContent is the regression for the churn: a feed
// that mints new UIDs on every render used to produce a full delete plus a full
// create on every sync.
func TestReissuedUIDsAreMatchedByContent(t *testing.T) {
	existing := []db.EventIdentity{
		row(1, "old-1", "Standup", 1000),
		row(2, "old-2", "Review", 2000),
		row(3, "old-3", "Retro", 3000),
	}
	// Same three meetings, brand new UIDs.
	parsed := []*models.CalendarEvent{
		feedEvent("new-1", "Standup", 1000),
		feedEvent("new-2", "Review", 2000),
		feedEvent("new-3", "Retro", 3000),
	}

	matches, creates, deletes := matchFeedEvents(existing, parsed)

	if len(creates) != 0 {
		t.Errorf("got %d creates, want 0 — nothing here is new", len(creates))
	}
	if len(deletes) != 0 {
		t.Errorf("got %d deletes, want 0 — nothing here is gone", len(deletes))
	}
	if len(matches) != 3 {
		t.Fatalf("got %d matches, want 3", len(matches))
	}

	seen := make(map[int64]string)
	for _, m := range matches {
		if !m.ByContent {
			t.Errorf("event %s should have been matched by content", m.Event.UID)
		}
		if prev, dup := seen[m.ExistingID]; dup {
			t.Errorf("row %d claimed twice: by %s and %s", m.ExistingID, prev, m.Event.UID)
		}
		seen[m.ExistingID] = m.Event.UID
	}
}

// TestAmbiguousContentIsNotGuessed: two meetings sharing a title and a start
// time cannot be told apart without the UID, so the matcher must decline rather
// than pair them arbitrarily.
func TestAmbiguousContentIsNotGuessed(t *testing.T) {
	existing := []db.EventIdentity{
		row(1, "old-1", "Interview", 1000),
		row(2, "old-2", "Interview", 1000),
	}
	parsed := []*models.CalendarEvent{
		feedEvent("new-1", "Interview", 1000),
		feedEvent("new-2", "Interview", 1000),
	}

	matches, creates, deletes := matchFeedEvents(existing, parsed)

	if len(matches) != 0 {
		t.Errorf("got %d matches, want 0 — the pairing is ambiguous", len(matches))
	}
	if len(creates) != 2 {
		t.Errorf("got %d creates, want 2", len(creates))
	}
	if len(deletes) != 2 {
		t.Errorf("got %d deletes, want 2", len(deletes))
	}
}

// TestUIDMatchWinsOverContent: a row must not be stolen by a content match when
// another event in the same feed owns it by UID, whatever order they arrive in.
func TestUIDMatchWinsOverContent(t *testing.T) {
	existing := []db.EventIdentity{
		row(1, "uid-a", "Standup", 1000),
	}
	parsed := []*models.CalendarEvent{
		// Comes first and would content-match row 1 if the passes were merged.
		feedEvent("reissued", "Standup", 1000),
		feedEvent("uid-a", "Standup", 1000),
	}

	matches, creates, _ := matchFeedEvents(existing, parsed)

	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(matches))
	}
	if matches[0].Event.UID != "uid-a" {
		t.Errorf("row 1 was claimed by %q, want the event that owns it by UID", matches[0].Event.UID)
	}
	if matches[0].ByContent {
		t.Error("the winning match should be a UID match, not a content match")
	}
	if len(creates) != 1 || creates[0].UID != "reissued" {
		t.Errorf("the other event should be created, got %d creates", len(creates))
	}
}

// TestMovedEventIsReplaced: shifting the start changes the identity, which is
// the intended trade-off — the alternative is matching so loosely that two
// different meetings merge.
func TestMovedEventIsReplaced(t *testing.T) {
	existing := []db.EventIdentity{row(1, "old-1", "Standup", 1000)}
	parsed := []*models.CalendarEvent{feedEvent("new-1", "Standup", 5000)}

	matches, creates, deletes := matchFeedEvents(existing, parsed)

	if len(matches) != 0 {
		t.Errorf("got %d matches, want 0", len(matches))
	}
	if len(creates) != 1 {
		t.Errorf("got %d creates, want 1", len(creates))
	}
	if len(deletes) != 1 || deletes[0] != "old-1" {
		t.Errorf("got deletes %v, want [old-1]", deletes)
	}
}

// TestGenuinelyNewAndGoneEvents keeps the plain bookkeeping honest.
func TestGenuinelyNewAndGoneEvents(t *testing.T) {
	existing := []db.EventIdentity{
		row(1, "uid-a", "Standup", 1000),
		row(2, "uid-gone", "Cancelled thing", 2000),
	}
	parsed := []*models.CalendarEvent{
		feedEvent("uid-a", "Standup", 1000),
		feedEvent("uid-fresh", "Brand new", 9000),
	}

	matches, creates, deletes := matchFeedEvents(existing, parsed)

	if len(matches) != 1 || matches[0].Event.UID != "uid-a" {
		t.Errorf("expected uid-a to match, got %d matches", len(matches))
	}
	if len(creates) != 1 || creates[0].UID != "uid-fresh" {
		t.Errorf("expected uid-fresh to be created, got %d creates", len(creates))
	}
	if len(deletes) != 1 || deletes[0] != "uid-gone" {
		t.Errorf("expected uid-gone to be deleted, got %v", deletes)
	}
}

// TestEmptyFeedDeletesNothingByItself documents that matchFeedEvents itself is
// not the guard: doSync refuses an empty parse before ever getting here. If that
// check is ever removed, this test shows what the matcher would do on its own.
func TestEmptyFeedWouldDeleteEverything(t *testing.T) {
	existing := []db.EventIdentity{row(1, "uid-a", "Standup", 1000)}

	_, _, deletes := matchFeedEvents(existing, nil)

	if len(deletes) != 1 {
		t.Errorf("got %d deletes, want 1 — the empty-feed guard lives in doSync", len(deletes))
	}
}

// TestSummaryWhitespaceIsIgnored: feeds re-wrap and re-pad titles between
// renders, which must not read as a different meeting.
func TestSummaryWhitespaceIsIgnored(t *testing.T) {
	existing := []db.EventIdentity{row(1, "old-1", "Standup", 1000)}
	parsed := []*models.CalendarEvent{feedEvent("new-1", "  Standup ", 1000)}

	matches, creates, deletes := matchFeedEvents(existing, parsed)

	if len(matches) != 1 || !matches[0].ByContent {
		t.Errorf("padding around the title broke the content match: %d matches, %d creates, %d deletes",
			len(matches), len(creates), len(deletes))
	}
}
