package client

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/parser"
	msgsvc "github.com/ddletotam/ddmailserver/internal/service/messages"
	"github.com/emersion/go-imap"
)

// Mirroring upstream deletions.
//
// A message deleted on the source server (or moved to its Trash, which we do
// not sync) used to stay in the local copy forever. Each folder run reports
// the account's rows that pointed at the folder before the run and are no
// longer there (folderPresence); after ALL folders of the account are synced
// this pass decides:
//
//  1. Re-read every such row. If the run re-pointed it (the message showed up
//     in another synced folder or under a new UID — a move upstream, in either
//     folder order) or its Message-ID was seen anywhere this cycle, it moved.
//  2. Rows with an unpushed local change or delete (flag_sync_queue), rows
//     \Deleted or in a local Trash (the user's own delete is in flight) and
//     rows already in the vault are left alone.
//  3. Mass-deletion guard per folder: more than vanishGuardMin rows AND more
//     than half the folder's rows gone at once is far more likely a broken
//     server answer or a folder rename than a real delete — nothing is
//     deleted, a warning is logged for manual review.
//  4. Confirmation upstream: every remaining candidate is looked up by
//     Message-ID (`UID SEARCH HEADER Message-ID`) in every synced folder and
//     in All-Mail-style folders (Gmail archive = only in All Mail). Found →
//     the row is re-pointed there; any lookup error → the candidate waits for
//     the next cycle. Messages with a synthetic Message-ID can't be searched
//     and are deleted only when every synced folder completed this cycle.
//  5. What is left vanished: Service.DeleteVanishedUpstream (vault
//     soft-delete, no delete-sync upstream), IMAP sessions get EXPUNGE, the
//     desktop an `expunge` push (the change journal carries the tombstone).
//
// Absence is only ever proven by a complete answer: a folder run that failed
// anywhere, was cancelled, or had no trustworthy baseline (first pass, no
// UIDVALIDITY) contributes nothing. Under the same UIDVALIDITY absence is by
// UID (UIDs <= last_seen_uid not in `UID FETCH 1:last (UID FLAGS)`); after a
// UIDVALIDITY change it is by Message-ID over the full envelope scan.

const (
	// vanishGuardMin / the 50% rule: see step 3 above.
	vanishGuardMin = 20
	// vanishConfirmMax caps the upstream lookups per cycle (candidates ×
	// folders SEARCH commands); the rest waits for the next cycle.
	vanishConfirmMax = 200
)

// remoteAccount is the part of *Client the account-level sync drives; tests
// substitute a fake.
type remoteAccount interface {
	remoteMailbox
	ListFolders() ([]*imap.MailboxInfo, error)
	UIDSearch(criteria *imap.SearchCriteria) ([]uint32, error)
}

// ExpungeNotice tells the notification hub that messages left a local folder
// because they vanished upstream. SeqNums are the IMAP sequence numbers the
// rows had in that folder's client view, descending (each stays valid while
// the higher ones are applied).
type ExpungeNotice struct {
	Username string
	Mailbox  string
	FolderID int64
	SeqNums  []uint32
}

type presenceMode int

const (
	presenceNone        presenceMode = iota // the run proves nothing
	presenceByUID                           // same UIDVALIDITY: absence by UID
	presenceByMessageID                     // after a UIDVALIDITY change: absence by Message-ID
)

// folderPresence is what one completed folder run proved.
type folderPresence struct {
	name     string
	mode     presenceMode
	lastSeen uint32                         // presenceByUID: UIDs above it are not judged
	present  map[uint32]bool                // presenceByUID: UIDs <= lastSeen still on the server
	before   map[uint32]db.RemoteMessageRef // rows pointing at the folder before the run
}

// goneRef is a row whose remote message is not where it pointed.
type goneRef struct {
	id        int64
	messageID string
	folder    string
	uid       uint32
}

// gone lists the rows the run did not find. By Message-ID it is every row of
// the baseline; the decision drops those whose Message-ID was seen.
func (p folderPresence) gone() []goneRef {
	var out []goneRef
	switch p.mode {
	case presenceByUID:
		for uid, ref := range p.before {
			if uid <= p.lastSeen && !p.present[uid] {
				out = append(out, goneRef{id: ref.ID, messageID: ref.MessageID, folder: p.name, uid: uid})
			}
		}
	case presenceByMessageID:
		for uid, ref := range p.before {
			out = append(out, goneRef{id: ref.ID, messageID: ref.MessageID, folder: p.name, uid: uid})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// vanishStats is the account-log summary of one cycle.
type vanishStats struct {
	removed  int // soft-deleted as vanished upstream
	moved    int // found elsewhere upstream (re-pointed)
	held     int // stopped by the mass-deletion guard
	pending  int // left for later: unpushed local change, local delete in flight
	deferred int // unconfirmed (lookup failed, cap, incomplete cycle)
}

// vanishPlan is the DB-side decision before the upstream confirmation.
type vanishPlan struct {
	candidates []goneRef
	held       map[string]int // folder → rows stopped by the guard
	moved      int
	pending    int
}

// planVanished turns the folders' gone rows into delete candidates (steps 1–3
// of the file comment). cur is the re-read state of every gone row, live the
// account's live row count per remote folder.
func planVanished(accountID int64, presences []folderPresence, seen map[string]bool,
	cur map[int64]db.RemoteRowPointer, live map[string]int) vanishPlan {
	plan := vanishPlan{held: map[string]int{}}
	for _, p := range presences {
		var cands []goneRef
		for _, g := range p.gone() {
			r, ok := cur[g.id]
			if !ok || r.SoftDeleted || r.AccountID != accountID || r.RemoteUID == 0 {
				continue // gone from the DB, already in the vault, no longer ours
			}
			if r.RemoteFolder != g.folder || r.RemoteUID != g.uid {
				if r.RemoteFolder != g.folder {
					plan.moved++
				}
				continue // re-pointed this cycle: moved / renumbered upstream
			}
			if seen[g.messageID] {
				plan.moved++
				continue
			}
			if r.Pending || r.Deleted || r.FolderType == "trash" {
				plan.pending++
				continue
			}
			cands = append(cands, g)
		}
		if n := len(cands); n > vanishGuardMin && n*2 > live[p.name] {
			plan.held[p.name] = n
			continue
		}
		plan.candidates = append(plan.candidates, cands...)
	}
	return plan
}

// isSyntheticMessageID reports a Message-ID derived from the content (the
// message had none) — not searchable upstream.
func isSyntheticMessageID(id string) bool {
	return strings.HasSuffix(strings.TrimSuffix(strings.TrimSpace(id), ">"), "@"+parser.SyntheticMessageIDDomain)
}

// foundAt is where an upstream lookup found a candidate.
type foundAt struct {
	folder string
	uid    uint32
}

// confirmVanished looks the candidates up by Message-ID in the given remote
// folders (step 4). Returns where each found one is, and which could not be
// checked completely (any SELECT/SEARCH error, cancellation). Synthetic
// Message-IDs are not looked up — neither found nor unknown.
func confirmVanished(ctx context.Context, c remoteAccount, cands []goneRef, folders []string) (map[int64]foundAt, map[int64]bool) {
	found := map[int64]foundAt{}
	unknown := map[int64]bool{}
	var searchable []goneRef
	for _, g := range cands {
		if !isSyntheticMessageID(g.messageID) {
			searchable = append(searchable, g)
		}
	}
	for _, f := range folders {
		if len(searchable) == len(found) {
			break
		}
		if ctx.Err() != nil {
			markUnknown(searchable, found, unknown)
			break
		}
		if _, err := c.SelectFolder(f); err != nil {
			log.Printf("upstream-deletion check: select %q: %v", f, err)
			markUnknown(searchable, found, unknown)
			continue
		}
		for _, g := range searchable {
			if _, ok := found[g.id]; ok {
				continue
			}
			bare := strings.Trim(strings.TrimSpace(g.messageID), "<>")
			if bare == "" {
				unknown[g.id] = true
				continue
			}
			crit := imap.NewSearchCriteria()
			crit.Header.Add("Message-ID", bare)
			uids, err := c.UIDSearch(crit)
			if err != nil {
				log.Printf("upstream-deletion check: search %q in %q: %v", g.messageID, f, err)
				unknown[g.id] = true
				continue
			}
			var top uint32
			for _, u := range uids {
				if u > top {
					top = u
				}
			}
			if top > 0 {
				found[g.id] = foundAt{folder: f, uid: top}
			}
		}
	}
	for id := range found {
		delete(unknown, id) // found anywhere wins over a failed lookup elsewhere
	}
	return found, unknown
}

func markUnknown(cands []goneRef, found map[int64]foundAt, unknown map[int64]bool) {
	for _, g := range cands {
		if _, ok := found[g.id]; !ok {
			unknown[g.id] = true
		}
	}
}

// markSeen records that a Message-ID is present upstream in some synced
// folder this cycle.
func (t *SyncTask) markSeen(messageID string) {
	if messageID == "" {
		return
	}
	if t.seenIDs == nil {
		t.seenIDs = make(map[string]bool)
	}
	t.seenIDs[messageID] = true
}

// reconcileVanished is the account-level pass (see the file comment).
// listed is every mailbox LIST returned; synced the folders this cycle synced;
// allComplete whether every one of them completed without error.
func (t *SyncTask) reconcileVanished(ctx context.Context, c remoteAccount, listed []*imap.MailboxInfo,
	synced []string, presences []folderPresence, allComplete bool) vanishStats {
	var st vanishStats
	live, err := t.database.RemoteFolderLiveCounts(t.account.ID)
	if err != nil {
		log.Printf("Sync [%s]: upstream deletions skipped: %v", t.account.Email, err)
		return st
	}

	// Rows pointing at a remote folder that is no longer in LIST: a rename
	// or a deleted folder — never deleted automatically.
	listedNames := map[string]bool{}
	var allMail []string
	for _, mb := range listed {
		if mb == nil {
			continue
		}
		listedNames[mb.Name] = true
		if isAllMailbox(mb) {
			allMail = append(allMail, mb.Name)
		}
	}
	var missing []string
	for name, n := range live {
		if !listedNames[name] {
			missing = append(missing, fmt.Sprintf("%q (%d)", name, n))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.accountLog("warning", "WARNING: remote folders gone from LIST, their messages kept for manual review: %s",
			strings.Join(missing, ", "))
	}

	var ids []int64
	for _, p := range presences {
		for _, g := range p.gone() {
			ids = append(ids, g.id)
		}
	}
	if len(ids) == 0 {
		return st
	}
	cur, err := t.database.GetRemoteRowPointers(ids)
	if err != nil {
		log.Printf("Sync [%s]: upstream deletions skipped: %v", t.account.Email, err)
		return st
	}
	plan := planVanished(t.account.ID, presences, t.seenIDs, cur, live)
	st.moved, st.pending = plan.moved, plan.pending
	for name, n := range plan.held {
		st.held += n
		t.accountLog("warning", "WARNING: %d of %d messages of remote folder %q vanished in one cycle — "+
			"not deleting (mass-deletion guard), review manually", n, live[name], name)
	}
	cands := plan.candidates
	if len(cands) > vanishConfirmMax {
		st.deferred += len(cands) - vanishConfirmMax
		cands = cands[:vanishConfirmMax]
	}
	if len(cands) == 0 || ctx.Err() != nil {
		st.deferred += len(cands)
		return st
	}

	searchIn := append(append([]string(nil), synced...), allMail...)
	found, unknown := confirmVanished(ctx, c, cands, searchIn)
	if ctx.Err() != nil {
		st.deferred += len(cands)
		return st
	}
	var victims []goneRef
	for _, g := range cands {
		if at, ok := found[g.id]; ok {
			if _, err := t.database.RepointRemoteMessage(g.id, t.account.ID, g.folder, g.uid, at.folder, at.uid); err != nil {
				log.Printf("Sync [%s]: repoint %s: %v", t.account.Email, g.messageID, err)
			}
			st.moved++
			continue
		}
		if unknown[g.id] || (isSyntheticMessageID(g.messageID) && !allComplete) {
			st.deferred++
			continue
		}
		victims = append(victims, g)
	}
	t.deleteVanished(ctx, victims, cur, &st)
	return st
}

// deleteVanished runs the confirmed deletes through the message service and
// tells the user's clients.
func (t *SyncTask) deleteVanished(ctx context.Context, victims []goneRef, cur map[int64]db.RemoteRowPointer, st *vanishStats) {
	if len(victims) == 0 {
		return
	}
	// IMAP sequence numbers must be taken before the rows leave the view.
	byFolder := map[int64][]uint32{}
	for _, g := range victims {
		r := cur[g.id]
		byFolder[r.FolderID] = append(byFolder[r.FolderID], r.UID)
	}
	seqBefore := map[int64]map[uint32]uint32{}
	for folderID, uids := range byFolder {
		m, err := t.database.FolderClientSeqNums(folderID, uids)
		if err != nil {
			log.Printf("Sync [%s]: seqnums of folder %d: %v", t.account.Email, folderID, err)
			continue
		}
		seqBefore[folderID] = m
	}

	svc := msgsvc.NewWithDB(t.database)
	removed := map[int64][]uint32{} // local folder → seqnums of deleted rows
	touched := map[int64]bool{}
	for _, g := range victims {
		if ctx.Err() != nil {
			st.deferred++
			continue
		}
		res, err := svc.DeleteVanishedUpstream(ctx, t.account.UserID, msgsvc.VanishedRef{
			MessageID: g.id, AccountID: t.account.ID, RemoteFolder: g.folder, RemoteUID: g.uid,
		})
		switch {
		case err != nil:
			log.Printf("Sync [%s]: delete vanished %s: %v", t.account.Email, g.messageID, err)
			st.deferred++
		case res.Deleted:
			st.removed++
			log.Printf("Sync [%s]: %s vanished from remote %q (uid %d) — moved to vault",
				t.account.Email, g.messageID, g.folder, g.uid)
			touched[res.FolderID] = true
			if seq, ok := seqBefore[res.FolderID][res.UID]; ok {
				removed[res.FolderID] = append(removed[res.FolderID], seq)
			}
		case res.Skip == msgsvc.VanishedSkipMoved:
			st.moved++
		case res.Skip == msgsvc.VanishedSkipPending || res.Skip == msgsvc.VanishedSkipDeletedFlag ||
			res.Skip == msgsvc.VanishedSkipTrash:
			st.pending++
		}
	}
	if len(touched) == 0 || t.expungeNotifyFunc == nil {
		return
	}
	user, err := t.database.GetUserByID(t.account.UserID)
	if err != nil {
		log.Printf("Sync [%s]: expunge notice: %v", t.account.Email, err)
		return
	}
	for folderID := range touched {
		folder, err := t.database.GetFolderByID(folderID)
		if err != nil {
			log.Printf("Sync [%s]: expunge notice: folder %d: %v", t.account.Email, folderID, err)
			continue
		}
		seqs := removed[folderID]
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] > seqs[j] })
		t.expungeNotifyFunc(ExpungeNotice{
			Username: user.Username, Mailbox: folder.Name, FolderID: folderID, SeqNums: seqs,
		})
	}
}

// isAllMailbox reports a Gmail-style "All Mail" folder: not synced (it
// duplicates every other folder) but where an archived message still lives.
func isAllMailbox(mb *imap.MailboxInfo) bool {
	for _, a := range mb.Attributes {
		if strings.EqualFold(a, `\All`) {
			return true
		}
	}
	return isAllMailName(strings.ToLower(mb.Name))
}
