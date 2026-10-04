// Package messages is the service layer for mutating mail messages: flags,
// deletion and the upstream sync those imply. It is the single place that
// decides when a local change must be pushed back to the source IMAP server
// (flag_sync_queue), so the IMAP server, the desktop API and the web UI cannot
// drift apart again — they used to carry three copies of that rule.
//
// Every mutation runs in one transaction: the message row is locked, changed,
// and the upstream sync entry queued atomically. A crash or error in between
// can no longer leave a local change that is never pushed (or a push of a
// change that never happened locally).
package messages

import (
	"context"
	"errors"
	"fmt"

	"github.com/ddletotam/ddmailserver/internal/db"
)

// ErrNotFound is returned when the message does not exist or belongs to
// another user.
var ErrNotFound = db.ErrNotFound

// Tx is the slice of a database transaction the service composes. *db.Tx
// implements it; tests use an in-memory fake.
type Tx interface {
	LockMessageForUpdate(userID, messageID int64) (*db.MessageSyncState, error)
	SetMessageFlags(messageID int64, f db.MessageFlags) error
	QueueFlagSync(messageID, accountID int64, remoteFolder string, remoteUID uint32, seen, flagged, answered, deleted bool) error
	SoftDeleteMessage(messageID int64) error
	LockPurgeTargets(userID int64, ids []int64, addresses, domains []string) ([]*db.MessageSyncState, error)
	HardDeleteUserMessages(userID int64, ids []int64) (int64, error)
}

// Store opens transactions.
type Store interface {
	InTx(ctx context.Context, fn func(Tx) error) error
}

type dbStore struct{ database *db.DB }

func (s dbStore) InTx(ctx context.Context, fn func(Tx) error) error {
	return s.database.InTx(ctx, func(tx *db.Tx) error { return fn(tx) })
}

// Service mutates messages. Safe for concurrent use.
type Service struct {
	store Store
}

// New returns a service over an arbitrary Store (tests).
func New(store Store) *Service {
	return &Service{store: store}
}

// NewWithDB returns a service backed by the database.
func NewWithDB(database *db.DB) *Service {
	return New(dbStore{database: database})
}

// Bool returns a pointer to b — shorthand for building a FlagUpdate.
func Bool(b bool) *bool { return &b }

// FlagUpdate lists the flags to change; nil fields are left as they are.
type FlagUpdate struct {
	Seen     *bool
	Flagged  *bool
	Answered *bool
	Deleted  *bool
	Draft    *bool
}

// Apply returns f with the update applied.
func (u FlagUpdate) Apply(f db.MessageFlags) db.MessageFlags {
	if u.Seen != nil {
		f.Seen = *u.Seen
	}
	if u.Flagged != nil {
		f.Flagged = *u.Flagged
	}
	if u.Answered != nil {
		f.Answered = *u.Answered
	}
	if u.Deleted != nil {
		f.Deleted = *u.Deleted
	}
	if u.Draft != nil {
		f.Draft = *u.Draft
	}
	return f
}

// Merge returns u with every field set in o overriding u's.
func (u FlagUpdate) Merge(o FlagUpdate) FlagUpdate {
	if o.Seen != nil {
		u.Seen = o.Seen
	}
	if o.Flagged != nil {
		u.Flagged = o.Flagged
	}
	if o.Answered != nil {
		u.Answered = o.Answered
	}
	if o.Deleted != nil {
		u.Deleted = o.Deleted
	}
	if o.Draft != nil {
		u.Draft = o.Draft
	}
	return u
}

// FlagUpdateFor maps an IMAP system flag name (`\Seen`, `\Flagged`,
// `\Answered`, `\Deleted`, `\Draft`) to an update that sets it to value.
// ok is false for any other name (keywords, `\Recent`).
func FlagUpdateFor(flag string, value bool) (u FlagUpdate, ok bool) {
	v := Bool(value)
	switch flag {
	case `\Seen`:
		u.Seen = v
	case `\Flagged`:
		u.Flagged = v
	case `\Answered`:
		u.Answered = v
	case `\Deleted`:
		u.Deleted = v
	case `\Draft`:
		u.Draft = v
	default:
		return FlagUpdate{}, false
	}
	return u, true
}

// FlagResult describes the outcome of SetFlags.
type FlagResult struct {
	Before db.MessageFlags
	After  db.MessageFlags
	// Queued reports that the change was queued for the source server.
	Queued bool
}

// Changed reports whether any flag actually moved.
func (r FlagResult) Changed() bool { return r.Before != r.After }

// VisibleChanged reports whether a flag the user sees as unread/starred/
// replied state moved (\Seen, \Flagged, \Answered) — the changes worth a push
// to the user's other clients.
func (r FlagResult) VisibleChanged() bool {
	return r.Before.Seen != r.After.Seen ||
		r.Before.Flagged != r.After.Flagged ||
		r.Before.Answered != r.After.Answered
}

// upstreamSynced reports whether the message mirrors a message on an external
// IMAP account whose UID we know — only then is there something to push.
// AccountID 0 is a message delivered by our own MX; RemoteUID 0 means the
// source UID was never learned (rows older than remote_uid tracking).
func upstreamSynced(st *db.MessageSyncState) bool {
	return st.AccountID > 0 && st.RemoteUID > 0
}

// syncedFlagsMoved reports whether a flag the source server stores moved.
// \Draft is local-only. A no-op re-assert (a client marking an already-read
// message read again — Thunderbird does it on every open) must not be queued:
// it costs a pointless remote STORE, and a pending row freezes the flag
// columns against the upstream pull, delaying a genuine remote-side change.
func syncedFlagsMoved(before, after db.MessageFlags) bool {
	return before.Seen != after.Seen || before.Flagged != after.Flagged ||
		before.Answered != after.Answered || before.Deleted != after.Deleted
}

// SetFlags applies upd to the user's message and, for an external-account
// message, queues the new state for the source server. The upstream STORE is
// a SET (replace-all), so the full post-update state is queued, not the delta.
// A no-op update writes nothing.
func (s *Service) SetFlags(ctx context.Context, userID, messageID int64, upd FlagUpdate) (FlagResult, error) {
	var res FlagResult
	err := s.store.InTx(ctx, func(tx Tx) error {
		st, err := tx.LockMessageForUpdate(userID, messageID)
		if err != nil {
			return err
		}
		res.Before = st.Flags
		res.After = upd.Apply(st.Flags)
		if !res.Changed() {
			return nil
		}
		if err := tx.SetMessageFlags(st.ID, res.After); err != nil {
			return err
		}
		if upstreamSynced(st) && syncedFlagsMoved(res.Before, res.After) {
			a := res.After
			if err := tx.QueueFlagSync(st.ID, st.AccountID, st.RemoteFolder, st.RemoteUID,
				a.Seen, a.Flagged, a.Answered, a.Deleted); err != nil {
				return err
			}
			res.Queued = true
		}
		return nil
	})
	if err != nil {
		return FlagResult{}, wrap("set flags", messageID, err)
	}
	return res, nil
}

// DeleteResult describes the outcome of Delete.
type DeleteResult struct {
	// Queued reports that the delete was queued for the source server.
	Queued bool
}

// Delete moves the user's message to the vault (soft delete — recoverable,
// hidden from every folder view, purged later by the vault retention). For an
// external-account message the delete is also queued for the source server
// (STORE \Deleted + UID EXPUNGE there); otherwise the message would stay in
// the user's "real" mailbox forever and come back on the next full resync.
// This is what IMAP EXPUNGE does outside Trash plus the delete-sync that
// STORE \Deleted queues.
func (s *Service) Delete(ctx context.Context, userID, messageID int64) (DeleteResult, error) {
	var res DeleteResult
	err := s.store.InTx(ctx, func(tx Tx) error {
		st, err := tx.LockMessageForUpdate(userID, messageID)
		if err != nil {
			return err
		}
		if err := tx.SoftDeleteMessage(st.ID); err != nil {
			return err
		}
		if upstreamSynced(st) {
			if err := queueUpstreamDelete(tx, st); err != nil {
				return err
			}
			res.Queued = true
		}
		return nil
	})
	if err != nil {
		return DeleteResult{}, wrap("delete", messageID, err)
	}
	return res, nil
}

// PurgeSelector picks the messages a purge removes: explicit ids plus every
// message from one of the sender addresses or sender domains. Addresses and
// domains are matched literally (LIKE metacharacters are escaped).
type PurgeSelector struct {
	IDs             []int64
	SenderAddresses []string
	SenderDomains   []string
}

// PurgeResult describes the outcome of Purge.
type PurgeResult struct {
	Deleted int64
	// Queued counts deletes queued for source servers.
	Queued int
}

// Purge permanently deletes the selected messages of the user — no vault.
// It exists for the "this sender is spam, I want it gone everywhere" action,
// where keeping the rows in the vault is explicitly not wanted. External-
// account messages get their delete queued for the source server in the same
// transaction (the queue row outlives the message row, see migration 050).
func (s *Service) Purge(ctx context.Context, userID int64, sel PurgeSelector) (PurgeResult, error) {
	var res PurgeResult
	if len(sel.IDs) == 0 && len(sel.SenderAddresses) == 0 && len(sel.SenderDomains) == 0 {
		return res, nil
	}
	err := s.store.InTx(ctx, func(tx Tx) error {
		targets, err := tx.LockPurgeTargets(userID, sel.IDs, sel.SenderAddresses, sel.SenderDomains)
		if err != nil {
			return err
		}
		ids := make([]int64, 0, len(targets))
		queued := 0
		for _, st := range targets {
			ids = append(ids, st.ID)
			if upstreamSynced(st) {
				if err := queueUpstreamDelete(tx, st); err != nil {
					return err
				}
				queued++
			}
		}
		n, err := tx.HardDeleteUserMessages(userID, ids)
		if err != nil {
			return err
		}
		res = PurgeResult{Deleted: n, Queued: queued}
		return nil
	})
	if err != nil {
		return PurgeResult{}, fmt.Errorf("purge messages of user %d: %w", userID, err)
	}
	return res, nil
}

func queueUpstreamDelete(tx Tx, st *db.MessageSyncState) error {
	f := st.Flags
	return tx.QueueFlagSync(st.ID, st.AccountID, st.RemoteFolder, st.RemoteUID,
		f.Seen, f.Flagged, f.Answered, true)
}

func wrap(op string, messageID int64, err error) error {
	if errors.Is(err, ErrNotFound) {
		return ErrNotFound
	}
	return fmt.Errorf("%s message %d: %w", op, messageID, err)
}
