package messages

import (
	"context"
	"fmt"
)

// Transferred is the outcome of COPY/MOVE for one message.
type Transferred struct {
	MessageID int64  // the source row
	SrcUID    uint32 // its UID in the source folder
	DestUID   uint32 // its UID in the destination folder
	// Removed reports that the message left the source folder (it was moved,
	// not copied), so sessions on the source must be told EXPUNGE.
	Removed bool
	// Queued reports that a delete was queued for the source server.
	Queued bool
}

// Transfer copies (move=false) or moves the user's messages into the user's
// folder destFolderID — IMAP COPY and MOVE — in one transaction: either every
// message lands or nothing changes.
//
// A message is identified by (user, Message-ID) and exists once (migrations
// 041/042: the desktop contract and the change journal key on it), so a
// message that has a Message-ID cannot be duplicated. Its COPY is a move: the
// row goes to the destination folder under a new UID and leaves the source
// (Removed). That is what IMAP clients need from COPY in practice — a client
// without MOVE does COPY + STORE \Deleted + EXPUNGE, which then finds nothing
// left to delete — and the message is never lost; an explicit "keep in both
// folders" is the one thing that cannot be had. Messages without a Message-ID
// (local drafts) are copied for real.
//
// A message already in the destination stays where it is (DestUID = SrcUID).
//
// Moving into a Trash folder is a delete as far as the source server is
// concerned — it is how Thunderbird and iOS delete — so for an external-account
// message the upstream delete (STORE \Deleted + UID EXPUNGE there) is queued in
// the same transaction, like Delete does. The local row stays in Trash until
// it is expunged. Moves elsewhere are local organisation and touch nothing
// upstream.
func (s *Service) Transfer(ctx context.Context, userID int64, ids []int64, destFolderID int64, move bool) ([]Transferred, error) {
	var out []Transferred
	err := s.store.InTx(ctx, func(tx Tx) error {
		out = out[:0]
		for _, id := range ids {
			st, err := tx.LockMessageForUpdate(userID, id)
			if err != nil {
				return wrap("transfer", id, err)
			}
			t := Transferred{MessageID: st.ID, SrcUID: st.UID}
			relocate := move || st.MessageID != ""
			if st.FolderID == destFolderID && relocate {
				t.DestUID = st.UID
				out = append(out, t)
				continue
			}
			uid, folderType, err := tx.ClaimFolderUID(userID, destFolderID)
			if err != nil {
				return fmt.Errorf("destination folder %d: %w", destFolderID, err)
			}
			t.DestUID = uid
			if !relocate {
				if _, err := tx.CopyMessageRow(st.ID, destFolderID, uid); err != nil {
					return wrap("copy", id, err)
				}
				out = append(out, t)
				continue
			}
			if err := tx.MoveMessageRow(st.ID, destFolderID, uid); err != nil {
				return wrap("move", id, err)
			}
			t.Removed = true
			if folderType == "trash" && upstreamSynced(st) {
				if err := queueUpstreamDelete(tx, st); err != nil {
					return wrap("move", id, err)
				}
				t.Queued = true
			}
			out = append(out, t)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
