package messages

import (
	"context"
	"errors"
)

// VanishedRef names a local row the sync found gone from the source server,
// together with the upstream pointer the decision was based on.
type VanishedRef struct {
	MessageID    int64 // messages.id
	AccountID    int64
	RemoteFolder string
	RemoteUID    uint32
}

// Reasons DeleteVanishedUpstream leaves a message alone.
const (
	VanishedSkipMoved       = "moved"           // pointer changed since the decision
	VanishedSkipGone        = "gone"            // row no longer exists
	VanishedSkipVault       = "already-deleted" // already in the vault
	VanishedSkipPending     = "pending"         // unpushed local change / delete for it
	VanishedSkipDeletedFlag = "deleted-flag"    // \Deleted set locally, the client's EXPUNGE decides
	VanishedSkipTrash       = "local-trash"     // lives in a local Trash folder, expunge decides
)

// VanishedResult describes the outcome of DeleteVanishedUpstream.
type VanishedResult struct {
	Deleted bool
	Skip    string // why not, when !Deleted
	// Where the row was (for the IMAP EXPUNGE notification).
	FolderID int64
	UID      uint32
}

// DeleteVanishedUpstream is the delete for a message that disappeared from
// the source server (deleted or moved to Trash there): the same vault
// soft-delete as Delete — recoverable, hidden from every view, journaled for
// the desktop by the message_changes trigger — but nothing is queued for the
// source server, the copy there is already gone (queuing would only loop).
//
// The remote UID is cleared in the same transaction: a row restored from the
// vault then counts as local-only and is not deleted again by the next sync.
//
// Everything the sync decided on is re-checked under the row lock: the row
// must still point at ref's (account, folder, UID), must not have an unpushed
// local change or delete, and must not sit \Deleted or in a local Trash (the
// user is already deleting it — the client's EXPUNGE decides there). Any
// mismatch leaves the row untouched and reports why.
func (s *Service) DeleteVanishedUpstream(ctx context.Context, userID int64, ref VanishedRef) (VanishedResult, error) {
	var res VanishedResult
	err := s.store.InTx(ctx, func(tx Tx) error {
		res = VanishedResult{}
		st, err := tx.LockMessageForUpdate(userID, ref.MessageID)
		if err != nil {
			return err
		}
		res.FolderID, res.UID = st.FolderID, st.UID
		if st.AccountID != ref.AccountID || st.RemoteFolder != ref.RemoteFolder || st.RemoteUID != ref.RemoteUID {
			res.Skip = VanishedSkipMoved
			return nil
		}
		g, err := tx.UpstreamVanishGuard(st.ID)
		if err != nil {
			return err
		}
		switch {
		case g.SoftDeleted:
			res.Skip = VanishedSkipVault
		case g.Pending:
			res.Skip = VanishedSkipPending
		case st.Flags.Deleted:
			res.Skip = VanishedSkipDeletedFlag
		case g.FolderType == "trash":
			res.Skip = VanishedSkipTrash
		}
		if res.Skip != "" {
			return nil
		}
		if err := tx.SoftDeleteMessage(st.ID); err != nil {
			return err
		}
		if err := tx.ClearRemoteUID(st.ID); err != nil {
			return err
		}
		res.Deleted = true
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		return VanishedResult{Skip: VanishedSkipGone}, nil
	}
	if err != nil {
		return VanishedResult{}, wrap("delete vanished", ref.MessageID, err)
	}
	return res, nil
}
