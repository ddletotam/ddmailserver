package client

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/parser"
	"github.com/emersion/go-imap"
)

// Incremental pull of one remote mailbox.
//
// The bookmark (remote_folder_state: UIDVALIDITY + last_seen_uid) splits the
// mailbox in two:
//
//   - UIDs <= last_seen_uid are already processed. They get a light
//     `UID FETCH 1:last (UID FLAGS)`; each UID is resolved to its local row
//     through messages(account_id, remote_folder, remote_uid) and run through
//     the same dedup-hit path as before (flags + spam re-evaluation, pending
//     flag_sync protection inside RefreshExistingFromRemote).
//   - UIDs > last_seen_uid are scanned with `UID FETCH last+1:* (UID FLAGS
//     ENVELOPE)`; messages whose Message-ID is already known (moved between
//     remote folders, delivered to several sources) take the dedup-hit path
//     without a body; only unknown ones — and ones without a Message-ID, whose
//     derived id needs the body — are fetched with BODY.PEEK[].
//
// No bookmark or a changed UIDVALIDITY means a full pass: the scan above over
// 1:*, i.e. the old Message-ID dedup, but envelopes first and bodies only for
// unknown messages. The bookmark is written after the pass.
//
// Every successful run also reports which already-known remote messages of
// the folder are no longer there (folderPresence); the account-level pass in
// sync_vanished.go decides, after all folders, which of them really vanished
// upstream and mirrors that as a vault delete.

// remoteMailbox is the part of *Client the per-folder sync drives; tests
// substitute a fake.
type remoteMailbox interface {
	SelectFolder(name string) (*imap.MailboxStatus, error)
	FetchMessagesByUID(uidSet *imap.SeqSet, items []imap.FetchItem) (chan *imap.Message, chan error)
}

// folderPlan is what the bookmark allows for one run over a folder.
type folderPlan struct {
	full     bool   // scan 1:* with Message-ID dedup
	reason   string // why the pass is full (log); "" when incremental
	lastSeen uint32 // trusted bookmark; 0 on a full pass
	persist  bool   // a bookmark may be written after the run
}

// planFolderSync decides between an incremental and a full pass.
// uidValidity is what SELECT reported now.
func planFolderSync(st *db.RemoteFolderState, uidValidity uint32) folderPlan {
	switch {
	case uidValidity == 0:
		// Without UIDVALIDITY a UID means nothing across sessions — nothing
		// may be bookmarked, every run is a full (envelope-first) pass.
		return folderPlan{full: true, reason: "server reports no UIDVALIDITY"}
	case st == nil:
		return folderPlan{full: true, reason: "no saved state", persist: true}
	case st.UIDValidity != uidValidity:
		return folderPlan{full: true, reason: fmt.Sprintf("UIDVALIDITY changed %d→%d", st.UIDValidity, uidValidity), persist: true}
	}
	return folderPlan{lastSeen: st.LastSeenUID, persist: true}
}

// hasUIDsAbove reports whether the mailbox may hold UIDs above lastSeen.
// UIDNEXT 0 means the server didn't say — assume yes.
func hasUIDsAbove(lastSeen, uidNext uint32) bool {
	return uidNext == 0 || uidNext > lastSeen+1
}

// nextLastSeen computes the bookmark after a run: everything scanned is
// processed, except that the bookmark must stay below the lowest UID whose
// save failed, so that message is retried next run. Never goes back below
// prev (prev is 0 on a full pass).
func nextLastSeen(prev, maxScanned uint32, failed []uint32) uint32 {
	last := prev
	if maxScanned > last {
		last = maxScanned
	}
	for _, f := range failed {
		if f > prev && f-1 < last {
			last = f - 1
		}
	}
	return last
}

// fetchUIDRange runs one UID FETCH and keeps messages with lo <= UID <= hi
// (hi 0 = unbounded). The filter matters: per RFC 3501 `n:*` with n above
// the highest UID still returns the last message.
func fetchUIDRange(c remoteMailbox, set *imap.SeqSet, items []imap.FetchItem, lo, hi uint32) ([]*imap.Message, error) {
	ch, done := c.FetchMessagesByUID(set, items)
	var out []*imap.Message
	for m := range ch {
		if m == nil || m.Uid == 0 || m.Uid < lo || (hi > 0 && m.Uid > hi) {
			continue
		}
		out = append(out, m)
	}
	if err := <-done; err != nil {
		return out, err
	}
	return out, nil
}

// folderSyncResult is what one folder run contributes to the account log.
type folderSyncResult struct {
	newCount, skipped, spam int
	bodies                  int    // messages fetched with BODY.PEEK[]
	fullReason              string // non-empty when the folder got a full pass
	// presence is what the run proved about the folder's known messages;
	// mode presenceNone unless the run completed without any error.
	presence folderPresence
}

// syncOneFolder pulls one remote mailbox incrementally (see the file
// comment) and dispatches new messages through saveMessageToInbox with the
// appropriate folder role.
//
// before is the account's rows pointing at this folder, snapshotted at the
// start of the cycle (before any folder of the cycle could re-point them);
// nil = snapshot it now.
func (t *SyncTask) syncOneFolder(ctx context.Context, c remoteMailbox, localInbox *models.Folder, name string, class folderClass, before map[uint32]db.RemoteMessageRef) (folderSyncResult, error) {
	var res folderSyncResult
	mbox, err := c.SelectFolder(name)
	if err != nil {
		return res, err
	}
	st, err := t.database.GetRemoteFolderState(t.account.ID, name)
	if err != nil {
		// Unknown state is no state: a full pass is always correct.
		log.Printf("Sync [%s] %s: %v — full pass", t.account.Email, name, err)
		st = nil
	}
	plan := planFolderSync(st, mbox.UidValidity)
	if plan.full {
		res.fullReason = plan.reason
	}
	// Rows pointing at this folder before the run: the baseline the
	// vanished-upstream check compares against. Absence can be proven by UID
	// under the same UIDVALIDITY, or by Message-ID after a full pass that
	// follows a UIDVALIDITY change. A first pass (no state) or a server
	// without UIDVALIDITY proves nothing.
	byUID := !plan.full
	byMessageID := plan.full && st != nil && mbox.UidValidity != 0
	if (byUID || byMessageID) && before == nil {
		if before, err = t.database.GetRemoteMessageRefs(t.account.ID, name); err != nil {
			log.Printf("Sync [%s] %s: %v — upstream deletions not checked this run", t.account.Email, name, err)
			before, byUID, byMessageID = nil, false, false
		}
	}
	present := map[uint32]bool{}
	proven := func() folderPresence {
		switch {
		case byUID:
			return folderPresence{name: name, mode: presenceByUID, lastSeen: plan.lastSeen, present: present, before: before, junk: class == folderJunk}
		case byMessageID:
			return folderPresence{name: name, mode: presenceByMessageID, before: before, junk: class == folderJunk}
		}
		return folderPresence{name: name}
	}
	save := func(last uint32) {
		if !plan.persist {
			return
		}
		if st != nil && st.UIDValidity == mbox.UidValidity && st.LastSeenUID == last {
			return
		}
		if err := t.database.SaveRemoteFolderState(t.account.ID, name, db.RemoteFolderState{
			UIDValidity: mbox.UidValidity, LastSeenUID: last,
		}); err != nil {
			log.Printf("Sync [%s] %s: %v", t.account.Email, name, err)
		}
	}

	if mbox.Messages == 0 {
		// Everything below UIDNEXT is gone for good; anything new will get
		// a UID >= UIDNEXT.
		last := plan.lastSeen
		if mbox.UidNext > 0 && mbox.UidNext-1 > last {
			last = mbox.UidNext - 1
		}
		save(last)
		if byUID {
			// SELECT said 0 messages: every known UID is gone.
			res.presence = proven()
		}
		return res, nil
	}

	if !plan.full && plan.lastSeen > 0 {
		got, err := t.refreshFlagsUpTo(ctx, c, name, class, plan.lastSeen, before)
		if err != nil {
			// Flags are refreshed again next run; new mail still matters.
			// Without the full UID list nothing may be judged gone.
			log.Printf("Sync [%s] %s: flag refresh failed: %v", t.account.Email, name, err)
			byUID = false
		} else {
			present = got
		}
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if !hasUIDsAbove(plan.lastSeen, mbox.UidNext) {
			res.presence = proven()
			return res, nil
		}
	}

	// Scan: envelopes of everything above the bookmark (1:* on a full pass).
	scanSet := new(imap.SeqSet)
	scanSet.AddRange(plan.lastSeen+1, 0)
	scanned, err := fetchUIDRange(c, scanSet,
		[]imap.FetchItem{imap.FetchUid, imap.FetchFlags, imap.FetchEnvelope}, plan.lastSeen+1, 0)
	if err != nil {
		return res, fmt.Errorf("IMAP envelope fetch on %q failed: %w", name, err)
	}
	var maxScanned uint32
	var failed []uint32
	needBody := new(imap.SeqSet)
	for _, m := range scanned {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if m.Uid > maxScanned {
			maxScanned = m.Uid
		}
		want, err := t.triageEnvelope(m, name, class)
		switch {
		case err != nil:
			log.Printf("Sync [%s] %s: triage uid %d failed: %v", t.account.Email, name, m.Uid, err)
			failed = append(failed, m.Uid)
		case want:
			needBody.AddNum(m.Uid)
		default:
			res.skipped++
		}
	}

	// Bodies only for what isn't known locally.
	if !needBody.Empty() {
		section := &imap.BodySectionName{Peek: true}
		items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags, imap.FetchUid, section.FetchItem()}
		messages, fetchDone := c.FetchMessagesByUID(needBody, items)
		for msg := range messages {
			// Keep draining even after cancellation: the fetch goroutine
			// blocks on a full channel otherwise.
			if ctx.Err() != nil || msg == nil || !needBody.Contains(msg.Uid) {
				continue
			}
			res.bodies++
			saved, isSpam, err := t.saveMessageToInbox(msg, localInbox, name, class)
			if err != nil {
				log.Printf("Sync [%s] %s: save failed: %v", t.account.Email, name, err)
				failed = append(failed, msg.Uid)
				continue
			}
			if saved {
				res.newCount++
				if isSpam {
					res.spam++
				}
			} else {
				res.skipped++
			}
		}
		if err := <-fetchDone; err != nil {
			return res, fmt.Errorf("IMAP fetch on %q failed: %w", name, err)
		}
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
	}
	save(nextLastSeen(plan.lastSeen, maxScanned, failed))
	if len(failed) > 0 {
		// A message that failed triage/save was not recorded as seen: by
		// Message-ID it would look gone.
		byMessageID = false
	}
	res.presence = proven()
	return res, nil
}

// triageEnvelope runs the envelope-only part of saveMessageToInbox. Known
// messages (by Message-ID) take the dedup-hit path right here; wantBody is
// true for messages the body fetch must handle — unknown ones and ones
// without a Message-ID (their id is derived from the body).
func (t *SyncTask) triageEnvelope(m *imap.Message, remoteFolder string, class folderClass) (wantBody bool, err error) {
	if m.Envelope == nil {
		return false, nil
	}
	if len(m.Envelope.From) == 0 && m.Envelope.Subject == "" {
		return false, nil
	}
	if hasFlag(m.Flags, imap.DeletedFlag) {
		// Still there (about to be expunged upstream) — not vanished yet.
		t.markSeen(m.Envelope.MessageId)
		return false, nil
	}
	messageID := m.Envelope.MessageId
	if messageID == "" {
		return true, nil
	}
	exists, err := t.database.MessageExistsByMessageID(t.account.UserID, messageID)
	if err != nil {
		return false, err
	}
	if !exists {
		return true, nil
	}
	fromAddr := parser.SanitizeUTF8(formatAddressList(m.Envelope.From))
	t.refreshKnownMessage(messageID, fromAddr, m.Uid, m.Flags, remoteFolder, class)
	return false, nil
}

// refreshFlagsUpTo is the light pass over already-processed UIDs 1:lastSeen:
// FLAGS only, resolved to local rows by remote_uid (refs; loaded here when
// nil). UIDs with no local row of this account in this folder (skipped,
// deleted locally, or owned by another folder/source) are left alone — they
// were handled when first seen.
//
// It returns every UID <= lastSeen the server still has — the complete list,
// since the fetch either succeeded as a whole or returned an error — which is
// what the vanished-upstream check judges absence by.
func (t *SyncTask) refreshFlagsUpTo(ctx context.Context, c remoteMailbox, name string, class folderClass, lastSeen uint32, refs map[uint32]db.RemoteMessageRef) (map[uint32]bool, error) {
	set := new(imap.SeqSet)
	set.AddRange(1, lastSeen)
	msgs, err := fetchUIDRange(c, set, []imap.FetchItem{imap.FetchUid, imap.FetchFlags}, 1, lastSeen)
	if err != nil {
		return nil, fmt.Errorf("IMAP flags fetch on %q failed: %w", name, err)
	}
	present := make(map[uint32]bool, len(msgs))
	for _, m := range msgs {
		present[m.Uid] = true
	}
	if len(msgs) == 0 {
		return present, nil
	}
	if refs == nil {
		if refs, err = t.database.GetRemoteMessageRefs(t.account.ID, name); err != nil {
			return nil, err
		}
	}
	for _, m := range msgs {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if hasFlag(m.Flags, imap.DeletedFlag) {
			continue
		}
		ref, ok := refs[m.Uid]
		if !ok {
			continue
		}
		t.refreshKnownMessage(ref.MessageID, ref.From, m.Uid, m.Flags, name, class)
	}
	return present, nil
}

// spamRuleVerdict is one memoised CheckSpamRules answer.
type spamRuleVerdict struct {
	action string
	rule   *db.SpamRule
}

// checkSpamRulesMemo is CheckSpamRules memoised per run, keyed by the bare
// lower-cased address the rule lookup itself uses. Errors are not cached.
func (t *SyncTask) checkSpamRulesMemo(fromAddr string) (string, *db.SpamRule, error) {
	key := strings.ToLower(fromAddr)
	if lt := strings.LastIndex(key, "<"); lt >= 0 {
		if gt := strings.LastIndex(key, ">"); gt > lt {
			key = strings.TrimSpace(key[lt+1 : gt])
		}
	}
	if v, ok := t.spamRuleMemo[key]; ok {
		return v.action, v.rule, nil
	}
	action, rule, err := t.database.CheckSpamRules(t.account.UserID, fromAddr)
	if err != nil {
		return action, rule, err
	}
	if t.spamRuleMemo == nil {
		t.spamRuleMemo = make(map[string]spamRuleVerdict)
	}
	t.spamRuleMemo[key] = spamRuleVerdict{action: action, rule: rule}
	return action, rule, nil
}

// refreshKnownMessage is the dedup-hit path for a message whose row already
// exists: refresh remote pointer + flags, re-evaluate spam. Returns whether
// the message counts as spam.
func (t *SyncTask) refreshKnownMessage(messageID, fromAddr string, uid uint32, flags []string, remoteFolderName string, class folderClass) bool {
	t.markSeen(messageID)
	// Inbox path just refreshes the remote pointer if we never
	// recorded one. Spam path is more interesting: the upstream
	// provider has just (re-)classified an already-known message
	// as junk. Flip is_spam on the local row, unless the user has
	// a whitelist rule for that sender — that's the rescue path.
	if class == folderJunk {
		action, matchedRule, ruleErr := t.checkSpamRulesMemo(fromAddr)
		if ruleErr != nil {
			log.Printf("IMAP sync: check spam rules during reclassify: %v", ruleErr)
		}
		rescue := action == "allow"
		if rescue {
			log.Printf("IMAP sync: remote-spam %s rescued by whitelist rule %d", messageID, matchedRule.ID)
		} else {
			log.Printf("IMAP sync: remote-spam %s reclassified as spam (folder=%s)", messageID, remoteFolderName)
		}
		if err := t.database.ReclassifyMessageFromRemoteSpam(
			t.account.UserID, t.account.ID, messageID, uid, remoteFolderName, !rescue,
		); err != nil {
			log.Printf("IMAP sync: reclassify failed for %s: %v", messageID, err)
		}
		return !rescue
	}
	// Inbox path on an already-known row: sync flags from the
	// remote side AND decide is_spam based on the user's current
	// spam rules. Earlier this branch unconditionally downgraded
	// is_spam=false (to handle "user moved out of upstream Junk
	// back into INBOX") — which silently killed blacklisting:
	// the next sync after a Spam-button click would resurrect
	// every previously-blocked message because it sat in upstream
	// INBOX. We now re-evaluate the rule on every dedup hit so
	// blacklist verdicts stick across re-syncs and whitelist /
	// no-rule cases still rescue.
	ruleAction, ruleMatched, ruleErr := t.checkSpamRulesMemo(fromAddr)
	if ruleErr != nil {
		log.Printf("IMAP sync: check spam rules on existing %s: %v", messageID, ruleErr)
	}
	downgrade := ruleAction != "spam" // spam-rule keeps is_spam=true; allow / no-rule lets remote INBOX rescue
	changed, err := t.database.RefreshExistingFromRemote(
		t.account.UserID, t.account.ID, messageID, uid, remoteFolderName,
		hasFlag(flags, imap.SeenFlag),
		hasFlag(flags, imap.FlaggedFlag),
		hasFlag(flags, imap.AnsweredFlag),
		downgrade,
	)
	if err != nil {
		log.Printf("IMAP sync: refresh existing %s failed: %v", messageID, err)
	} else if changed {
		t.flagsChanged++
	}
	if !downgrade && ruleMatched != nil {
		// Make sure the row is actually flagged spam and that
		// spam_rule_id points at the rule. Cheap UPDATE; if the
		// row is already in this state, RowsAffected is 0.
		if err := t.database.ReclassifyMessageFromRemoteSpam(
			t.account.UserID, t.account.ID, messageID, uid, remoteFolderName, true,
		); err != nil {
			log.Printf("IMAP sync: re-flag spam on existing %s: %v", messageID, err)
		}
	}
	return ruleAction == "spam"
}
