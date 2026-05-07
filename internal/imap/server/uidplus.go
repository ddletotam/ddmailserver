package server

import (
	"fmt"
	"log"
	"strings"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/commands"
	imapserver "github.com/emersion/go-imap/server"
)

// UIDPLUSExtension implements RFC 4315 (UIDPLUS) and advertises SPECIAL-USE.
type UIDPLUSExtension struct{}

func NewUIDPLUSExtension() *UIDPLUSExtension {
	return &UIDPLUSExtension{}
}

func (ext *UIDPLUSExtension) Capabilities(c imapserver.Conn) []string {
	return []string{"UIDPLUS", "SPECIAL-USE"}
}

func (ext *UIDPLUSExtension) Command(name string) imapserver.HandlerFactory {
	// NOTE: go-imap v1 Enable() ignores extensions that handle UNSELECT, MOVE, or IDLE
	// (treats them as built-in). So we don't register MOVE here.
	// MOVE with COPYUID is handled via MoveMessagesUID in the existing Move handler fallback.
	switch strings.ToUpper(name) {
	case "APPEND":
		return func() imapserver.Handler { return &uidplusAppendHandler{} }
	case "COPY":
		return func() imapserver.Handler { return &uidplusCopyHandler{} }
	default:
		return nil
	}
}

// ── APPEND with APPENDUID ──

type uidplusAppendHandler struct {
	commands.Append
}

func (h *uidplusAppendHandler) Handle(conn imapserver.Conn) error {
	ctx := conn.Context()
	if ctx.User == nil {
		return imapserver.ErrNotAuthenticated
	}

	mbox, err := ctx.User.GetMailbox(h.Mailbox)
	if err == backend.ErrNoSuchMailbox {
		return &imap.ErrStatusResp{Resp: &imap.StatusResp{
			Type: imap.StatusRespNo,
			Code: imap.CodeTryCreate,
			Info: err.Error(),
		}}
	} else if err != nil {
		return err
	}

	// Try UIDPLUS-aware method first
	if mb, ok := mbox.(*Mailbox); ok {
		uid, uidValidity, err := mb.CreateMessageUID(h.Flags, h.Date, h.Message)
		if err != nil {
			if err == backend.ErrTooBig {
				return &imap.ErrStatusResp{Resp: &imap.StatusResp{
					Type: imap.StatusRespNo,
					Code: "TOOBIG",
					Info: "Message size exceeding limit",
				}}
			}
			return err
		}

		// Send EXISTS update if APPEND targets the selected mailbox
		if conn.Server().Updates == nil && ctx.Mailbox != nil && ctx.Mailbox.Name() == mbox.Name() {
			status, err := mbox.Status([]imap.StatusItem{imap.StatusMessages})
			if err == nil {
				status.Flags = nil
				status.PermanentFlags = nil
				status.UnseenSeqNum = 0
				conn.WriteResp(&imap.StatusResp{
					Type: imap.StatusRespOk,
					Code: imap.StatusRespCode(fmt.Sprintf("APPENDUID %d %d", uidValidity, uid)),
					Info: "APPEND completed",
				})
			}
		}

		log.Printf("UIDPLUS APPEND: uid=%d uidValidity=%d mailbox=%s", uid, uidValidity, h.Mailbox)
		return &imap.ErrStatusResp{Resp: &imap.StatusResp{
			Type: imap.StatusRespOk,
			Code: imap.StatusRespCode(fmt.Sprintf("APPENDUID %d %d", uidValidity, uid)),
			Info: "APPEND completed",
		}}
	}

	// Fallback: no UIDPLUS info
	return mbox.CreateMessage(h.Flags, h.Date, h.Message)
}

// ── COPY with COPYUID ──

type uidplusCopyHandler struct {
	commands.Copy
}

func (h *uidplusCopyHandler) Handle(conn imapserver.Conn) error {
	return h.handle(false, conn)
}

func (h *uidplusCopyHandler) UidHandle(conn imapserver.Conn) error {
	return h.handle(true, conn)
}

func (h *uidplusCopyHandler) handle(uid bool, conn imapserver.Conn) error {
	ctx := conn.Context()
	if ctx.Mailbox == nil {
		return imapserver.ErrNoMailboxSelected
	}

	if mb, ok := ctx.Mailbox.(*Mailbox); ok {
		uidValidity, srcUIDs, destUIDs, err := mb.CopyMessagesUID(uid, h.SeqSet, h.Mailbox)
		if err != nil {
			return err
		}
		if len(srcUIDs) > 0 {
			code := fmt.Sprintf("COPYUID %d %s %s", uidValidity, formatUIDSet(srcUIDs), formatUIDSet(destUIDs))
			log.Printf("UIDPLUS COPY: %s", code)
			return &imap.ErrStatusResp{Resp: &imap.StatusResp{
				Type: imap.StatusRespOk,
				Code: imap.StatusRespCode(code),
				Info: "COPY completed",
			}}
		}
		return nil
	}

	return ctx.Mailbox.CopyMessages(uid, h.SeqSet, h.Mailbox)
}

// NOTE: MOVE with COPYUID is NOT handled here because go-imap v1 Enable()
// silently ignores extensions that register MOVE/UNSELECT/IDLE commands.
// MOVE still works through the built-in handler → MoveMessages().
// COPYUID for MOVE can be added later if needed by patching go-imap.

// formatUIDSet formats a slice of UIDs as a comma-separated IMAP UID set string.
func formatUIDSet(uids []uint32) string {
	parts := make([]string, len(uids))
	for i, uid := range uids {
		parts[i] = fmt.Sprintf("%d", uid)
	}
	return strings.Join(parts, ",")
}

// Ensure interfaces
var _ imapserver.Extension = &UIDPLUSExtension{}
var _ imapserver.Handler = &uidplusAppendHandler{}
var _ imapserver.Handler = &uidplusCopyHandler{}
// COPY must support UID prefix
var _ imapserver.UidHandler = &uidplusCopyHandler{}
