package worker

import (
	"strconv"
	"strings"

	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
)

// eventMatch pairs a parsed feed event with the stored row it belongs to.
type eventMatch struct {
	Event      *models.CalendarEvent
	ExistingID int64

	// ByContent is set when the UID matched nothing and the row was recognised
	// by its content instead — that is, the feed reissued its UIDs.
	ByContent bool
}

// contentKey identifies "the same meeting at the same time" without leaning on
// the UID.
//
// Title and start are deliberately the whole of it. A feed that reissues UIDs
// still reproduces both, while location, description and end time are the
// fields an organiser edits — folding those in would turn an ordinary edit into
// a delete plus a create and take the event's local state with it.
//
// It is also the identity the desktop reminder scanner already uses to collapse
// duplicate toasts (occurrence_start_ms together with summary), so the two
// layers agree on what counts as one meeting.
func contentKey(summary string, dtstart int64) string {
	return strconv.FormatInt(dtstart, 10) + "\x00" + strings.TrimSpace(summary)
}

// matchFeedEvents works out, for one sync, which parsed events correspond to
// rows that already exist, which are genuinely new, and which stored rows the
// feed has stopped mentioning.
//
// UIDs are tried first and win outright. Only the leftovers are matched on
// content, and only where the pairing is unambiguous — exactly one candidate on
// each side — because a title and a start time are not a primary key: two
// distinct meetings can share both, and guessing there would merge them.
//
// The second pass exists because some feeds mint a new UID on every render.
// Without it, every event in such a calendar is deleted and recreated on each
// cycle, which resets the row ids, created_at, and the reminder cascades keyed
// to those ids — so a reminder can fire again after every sync.
func matchFeedEvents(existing []db.EventIdentity, parsed []*models.CalendarEvent) (matches []eventMatch, creates []*models.CalendarEvent, deleteUIDs []string) {
	byUID := make(map[string]*db.EventIdentity, len(existing))
	byContent := make(map[string][]*db.EventIdentity, len(existing))
	for i := range existing {
		row := &existing[i]
		byUID[row.UID] = row
		key := contentKey(row.Summary, row.DTStart)
		byContent[key] = append(byContent[key], row)
	}

	// How often each content key occurs in the feed itself: a key that appears
	// twice there identifies nothing.
	feedKeyCount := make(map[string]int, len(parsed))
	for _, event := range parsed {
		feedKeyCount[contentKey(event.Summary, event.DTStart)]++
	}

	claimed := make(map[int64]bool, len(existing))
	matchedByUID := make(map[*models.CalendarEvent]bool, len(parsed))

	// Pass one: UID matches, settled before content matching gets a chance to
	// claim a row that some later event owns outright.
	for _, event := range parsed {
		row, ok := byUID[event.UID]
		if !ok || claimed[row.ID] {
			continue
		}
		claimed[row.ID] = true
		matchedByUID[event] = true
		matches = append(matches, eventMatch{Event: event, ExistingID: row.ID})
	}

	// Pass two: everything the UID could not place.
	for _, event := range parsed {
		if matchedByUID[event] {
			continue
		}

		key := contentKey(event.Summary, event.DTStart)
		candidates := byContent[key]
		if feedKeyCount[key] == 1 && len(candidates) == 1 && !claimed[candidates[0].ID] {
			claimed[candidates[0].ID] = true
			matches = append(matches, eventMatch{Event: event, ExistingID: candidates[0].ID, ByContent: true})
			continue
		}

		creates = append(creates, event)
	}

	for i := range existing {
		if !claimed[existing[i].ID] {
			deleteUIDs = append(deleteUIDs, existing[i].UID)
		}
	}

	return matches, creates, deleteUIDs
}
