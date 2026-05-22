package web

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/lib/pq"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/parser"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// ── Auth ──

// HandleDesktopLogin authenticates via username+password and returns a JWT.
func (s *Server) HandleDesktopLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	user, err := s.database.GetUserByUsername(req.Username)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if !VerifyPassword(user.PasswordHash, req.Password) || user.IsBanned() {
		respondError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	token, err := GenerateToken(user.ID, user.Username, s.jwtSecret)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"token":    token,
		"user_id":  user.ID,
		"username": user.Username,
	})
}

// HandleDesktopRefresh issues a new JWT in exchange for any signature-valid
// token (even if expired). The user must still exist and not be banned.
func (s *Server) HandleDesktopRefresh(w http.ResponseWriter, r *http.Request) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		respondError(w, http.StatusUnauthorized, "missing authorization header")
		return
	}
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		respondError(w, http.StatusUnauthorized, "invalid authorization header")
		return
	}

	claims, err := ValidateTokenAllowExpired(parts[1], s.jwtSecret)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "invalid token")
		return
	}

	// Hard ceiling on how old a refreshable token can be. Without this any
	// signature-valid token, including one stolen from a decommissioned
	// laptop years ago, could be exchanged for a fresh one indefinitely.
	// 30 days keeps the "no forced re-login during normal use" property
	// while putting a floor under stale-credential exposure.
	const maxRefreshAge = 30 * 24 * time.Hour
	if claims.IssuedAt != nil && time.Since(claims.IssuedAt.Time) > maxRefreshAge {
		respondError(w, http.StatusUnauthorized, "token too old to refresh, please log in again")
		return
	}

	user, err := s.database.GetUserByID(claims.UserID)
	if err != nil || user.IsBanned() {
		respondError(w, http.StatusUnauthorized, "user not found or banned")
		return
	}

	token, err := GenerateToken(user.ID, user.Username, s.jwtSecret)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"token":    token,
		"user_id":  user.ID,
		"username": user.Username,
	})
}

// ── Desktop auth middleware (Bearer token → full User in context) ──

// DesktopAuthMiddleware validates a Bearer JWT and loads the full User object.
func (s *Server) DesktopAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			respondError(w, http.StatusUnauthorized, "missing authorization header")
			return
		}
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			respondError(w, http.StatusUnauthorized, "invalid authorization header")
			return
		}

		claims, err := ValidateToken(parts[1], s.jwtSecret)
		if err != nil {
			respondError(w, http.StatusUnauthorized, "invalid token")
			return
		}

		user, err := s.database.GetUserByID(claims.UserID)
		if err != nil || user.IsBanned() {
			respondError(w, http.StatusUnauthorized, "user not found or banned")
			return
		}

		ctx := r.Context()
		ctx = setUserContext(ctx, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ── Folders ──

// DesktopFolder is the JSON shape the desktop client expects.
type DesktopFolder struct {
	Name       string `json:"name"`
	Delimiter  string `json:"delimiter"`
	Unread     uint32 `json:"unread"`
	Total      uint32 `json:"total"`
	SpecialUse string `json:"special_use"`
}

func folderSpecialUse(folderType string) string {
	switch folderType {
	case "inbox":
		return "\\Inbox"
	case "sent":
		return "\\Sent"
	case "drafts":
		return "\\Drafts"
	case "trash":
		return "\\Trash"
	case "junk":
		return "\\Junk"
	case "archive":
		return "\\Archive"
	default:
		return ""
	}
}

// HandleDesktopFolders returns folders with unread/total counts.
func (s *Server) HandleDesktopFolders(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	folders, err := s.database.GetFoldersByUser(user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get folders")
		return
	}

	result := []DesktopFolder{}
	for _, f := range folders {
		total, unread, _ := s.database.GetFolderMessageCounts(f.ID)
		result = append(result, DesktopFolder{
			Name:       f.Name,
			Delimiter:  "/",
			Unread:     uint32(unread),
			Total:      uint32(total),
			SpecialUse: folderSpecialUse(f.Type),
		})
	}

	respondJSON(w, http.StatusOK, result)
}

// ── Search ──

// HandleDesktopSearch runs full-text search via Meilisearch.
func (s *Server) HandleDesktopSearch(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		respondJSON(w, http.StatusOK, []interface{}{})
		return
	}

	limitStr := r.URL.Query().Get("limit")
	limit := 30
	if n, err := strconv.Atoi(limitStr); err == nil && n > 0 && n <= 200 {
		limit = n
	}

	if s.searchIndexer == nil {
		respondError(w, http.StatusServiceUnavailable, "search not available")
		return
	}

	results, err := s.searchIndexer.Search(user.ID, query, limit, 0)
	if err != nil {
		log.Printf("Desktop search error: %v", err)
		respondError(w, http.StatusInternalServerError, "search failed")
		return
	}

	respondJSON(w, http.StatusOK, results.Hits)
}

// ── Messages ──

// HandleDesktopMessageSource returns the raw RFC-822 source of a message.
func (s *Server) HandleDesktopMessageSource(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	id, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid message id")
		return
	}

	msg, err := s.database.GetMessageByID(id)
	if err != nil || msg.UserID != user.ID {
		respondError(w, http.StatusNotFound, "message not found")
		return
	}

	w.Header().Set("Content-Type", "message/rfc822")

	// Prefer the original RFC-822 bytes (stored on receive in MX and on Sent
	// copy in saveToSentFolder via migration 032). Fall back to a stitched
	// reconstruction for legacy rows pre-dating that migration — at least
	// the user sees the headers and plain body instead of an empty modal.
	if raw, err := s.database.GetMessageRawEmail(msg.ID); err == nil && len(raw) > 0 {
		w.Write(raw)
		return
	}

	fmt.Fprintf(w, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: %s\r\n\r\n%s",
		msg.From, msg.To, msg.Subject, timeutil.FromMs(msg.Date).Format("Mon, 02 Jan 2006 15:04:05 -0700"),
		msg.MessageID, msg.Body)
}

// HandleDesktopMessagePart returns the binary content of an inline message part by Content-ID.
func (s *Server) HandleDesktopMessagePart(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	id, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid message id")
		return
	}

	msg, err := s.database.GetMessageByID(id)
	if err != nil || msg.UserID != user.ID {
		respondError(w, http.StatusNotFound, "message not found")
		return
	}

	cid := mux.Vars(r)["cid"]
	att, err := s.database.GetAttachmentByContentID(msg.ID, cid)
	if err != nil || att == nil || len(att.Data) == 0 {
		respondError(w, http.StatusNotFound, "part not found")
		return
	}

	w.Header().Set("Content-Type", att.ContentType)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Content-Length", strconv.Itoa(len(att.Data)))
	w.Write(att.Data)
}

// HandleDesktopAttachment streams the binary content of a non-inline
// attachment by its index — the same index the client received from
// HandleDesktopConversationMessages, which is the position in the
// `attachments` rows ordered by id. The endpoint sets a Content-
// Disposition so HTTP clients that follow the header get a sensible
// default filename (Rust side overrides this anyway).
func (s *Server) HandleDesktopAttachment(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	msgID, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid message id")
		return
	}
	index, err := strconv.Atoi(mux.Vars(r)["index"])
	if err != nil || index < 0 {
		respondError(w, http.StatusBadRequest, "invalid index")
		return
	}

	msg, err := s.database.GetMessageByID(msgID)
	if err != nil || msg.UserID != user.ID {
		respondError(w, http.StatusNotFound, "message not found")
		return
	}

	atts, err := s.database.GetAttachmentsByMessageID(msgID)
	if err != nil || index >= len(atts) {
		respondError(w, http.StatusNotFound, "attachment not found")
		return
	}
	meta := atts[index]
	full, err := s.database.GetAttachmentByID(meta.ID)
	if err != nil || full == nil || len(full.Data) == 0 {
		respondError(w, http.StatusNotFound, "attachment data missing")
		return
	}

	w.Header().Set("Content-Type", full.ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(full.Data)))
	// RFC 5987 encoding for non-ASCII filenames (e.g. Cyrillic). Mail
	// attachments routinely have Russian names — encoding via the
	// `filename*` param keeps them intact through any HTTP middleware.
	displayName := parser.DecodeMIMEHeader(full.Filename)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename*=UTF-8''%s`, url.PathEscape(displayName)))
	w.Write(full.Data)
}

// HandleDesktopSetFlags updates flags on messages.
// Accepts client format: {"messages":[{"folder":"X","uid":123},...], "flags":"\\Seen", "add":true}
// where "uid" is actually messages.id (DB primary key) in native mode.
func (s *Server) HandleDesktopSetFlags(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req struct {
		Messages []struct {
			Folder string `json:"folder"`
			UID    int64  `json:"uid"` // messages.id in native mode
		} `json:"messages"`
		Flags string `json:"flags"`
		Add   bool   `json:"add"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	for _, ref := range req.Messages {
		msg, err := s.database.GetMessageByID(ref.UID)
		if err != nil || msg.UserID != user.ID {
			continue
		}

		// Compute the post-update flag state up-front so we can queue
		// the remote sync with the right snapshot. StoreFlags on the
		// upstream is SET (replace-all), not ADD/REMOVE, so we have to
		// pass the full state — not just the bit that changed.
		newSeen, newFlagged, newAnswered := msg.Seen, msg.Flagged, msg.Answered
		propagate := false
		switch req.Flags {
		case "\\Seen":
			s.database.UpdateMessageFlag(ref.UID, "seen", req.Add)
			newSeen = req.Add
			propagate = true
		case "\\Flagged":
			s.database.UpdateMessageFlag(ref.UID, "flagged", req.Add)
			newFlagged = req.Add
			propagate = true
		case "\\Answered":
			s.database.UpdateMessageFlag(ref.UID, "answered", req.Add)
			newAnswered = req.Add
			propagate = true
		case "\\Deleted":
			// Explicit \Deleted via this endpoint is rare (the dedicated
			// /messages/delete handler is the normal path and proxies
			// the delete via DeleteMessageRemote). Keep the local DB
			// update for behavioural parity but don't queue here — the
			// dedicated handler is the single-source-of-truth for the
			// delete-on-remote dance.
			s.database.UpdateMessageFlag(ref.UID, "deleted", req.Add)
		case "\\Draft":
			s.database.UpdateMessageFlag(ref.UID, "draft", req.Add)
		}

		if propagate && msg.AccountID > 0 && msg.RemoteUID > 0 {
			if err := s.database.QueueFlagSync(
				msg.ID, msg.AccountID, msg.RemoteFolder, msg.RemoteUID,
				newSeen, newFlagged, newAnswered, false,
			); err != nil {
				log.Printf("desktop set-flags: queue flag sync failed for msg %d: %v", msg.ID, err)
			}
		}
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// HandleDesktopMarkSpamByDomain implements the sidebar's "Spam by domain"
// action: insert a domain-level spam rule, then flag every supplied message
// (an entire conversation, in practice) as spam. The rule covers future
// deliveries; the explicit message flagging covers the past so the conv
// disappears from the desktop immediately.
func (s *Server) HandleDesktopMarkSpamByDomain(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req struct {
		Domain   string `json:"domain"`
		Messages []struct {
			Folder string `json:"folder"`
			UID    int64  `json:"uid"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	domain := strings.ToLower(strings.TrimSpace(req.Domain))
	if domain == "" || !strings.Contains(domain, ".") {
		respondError(w, http.StatusBadRequest, "invalid domain")
		return
	}

	rule := &db.SpamRule{
		UserID:    user.ID,
		RuleType:  "domain",
		RuleValue: domain,
		Action:    "spam",
	}
	if err := s.database.CreateSpamRule(rule); err != nil {
		log.Printf("desktop spam-by-domain: create rule for %s: %v", domain, err)
		respondError(w, http.StatusInternalServerError, "create rule failed")
		return
	}

	marked := 0
	for _, m := range req.Messages {
		msg, err := s.database.GetMessageByID(m.UID)
		if err != nil || msg.UserID != user.ID {
			continue
		}
		if err := s.database.MarkMessageAsSpam(m.UID, &rule.ID); err != nil {
			log.Printf("desktop spam-by-domain: mark %d: %v", m.UID, err)
			continue
		}
		marked++
	}

	log.Printf("desktop spam-by-domain: domain=%s rule_id=%d marked=%d/%d", domain, rule.ID, marked, len(req.Messages))
	respondJSON(w, http.StatusOK, map[string]int{"rule_id": int(rule.ID), "marked": marked})
}

// HandleDesktopBlacklistAndPurge implements the chat-header "Spam"
// quick-action: blacklist the sender (domain or address) AND
// hard-DELETE every message from that sender for this user. Hard-
// delete, not soft — user intent is "this is unwanted and I don't
// want it anywhere, including the spam vault." Future arrivals from
// the same sender still get caught by the new spam rule and land
// is_spam=true at import time.
//
// Per-user: the spam rule is scoped to user_id, so no cross-account
// pollution. Existing rules for the same (rule_type, rule_value)
// upsert via the unique constraint, so this is idempotent on the
// rule side too.
func (s *Server) HandleDesktopBlacklistAndPurge(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req struct {
		Domain  string `json:"domain"`  // either domain OR address must be set
		Address string `json:"address"` // takes priority if both supplied
		// MessageIDs identifies the conversation rows the user is
		// looking at right now. Hard-deleted in addition to the from-
		// pattern sweep so outgoing-from-us threads (where from_addr
		// is OUR address, not the counterpart's) actually disappear.
		MessageIDs []int64 `json:"message_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	addr := strings.ToLower(strings.TrimSpace(req.Address))
	domain := strings.ToLower(strings.TrimSpace(req.Domain))
	if addr == "" && domain == "" {
		respondError(w, http.StatusBadRequest, "domain or address required")
		return
	}

	// Insert blacklist rule. Most specific available wins: an address
	// rule covers exactly one sender; a domain rule covers the whole
	// domain. The UI is expected to pass whichever the user clicked.
	var rule *db.SpamRule
	if addr != "" {
		rule = &db.SpamRule{UserID: user.ID, RuleType: "address", RuleValue: addr, Action: "spam"}
	} else {
		rule = &db.SpamRule{UserID: user.ID, RuleType: "domain", RuleValue: domain, Action: "spam"}
	}
	if err := s.database.CreateSpamRule(rule); err != nil {
		log.Printf("blacklist-and-purge: create rule failed: %v", err)
		respondError(w, http.StatusInternalServerError, "create rule failed")
		return
	}

	// Two deletion passes:
	//   1. By ID for the conversation the user is staring at. Covers
	//      outgoing-from-us threads where the from-pattern misses.
	//   2. By from-pattern across the user's whole mailbox. Covers
	//      historical inbound from the same sender that wasn't in
	//      the current conversation view.
	totalDeleted := int64(0)
	if len(req.MessageIDs) > 0 {
		res, err := s.database.Exec(
			`DELETE FROM messages WHERE user_id = $1 AND id = ANY($2)`,
			user.ID, pq.Array(req.MessageIDs),
		)
		if err != nil {
			log.Printf("blacklist-and-purge: id-delete failed: %v", err)
		} else {
			n, _ := res.RowsAffected()
			totalDeleted += n
		}
	}
	var pattern string
	if addr != "" {
		pattern = "%<" + addr + ">%"
	} else {
		pattern = "%@" + domain + ">%"
	}
	res, err := s.database.Exec(
		`DELETE FROM messages WHERE user_id = $1 AND from_addr ILIKE $2`,
		user.ID, pattern,
	)
	if err != nil {
		log.Printf("blacklist-and-purge: pattern-delete failed: %v", err)
	} else {
		n, _ := res.RowsAffected()
		totalDeleted += n
	}
	log.Printf("blacklist-and-purge: user=%d rule=%d (%s=%s) deleted=%d", user.ID, rule.ID, rule.RuleType, rule.RuleValue, totalDeleted)
	respondJSON(w, http.StatusOK, map[string]any{"rule_id": rule.ID, "deleted": totalDeleted})
}

// HandleDesktopDeleteMessages soft-deletes a batch of messages by ID. Used by
// the "Delete conversation" action in the desktop sidebar — soft delete keeps
// the rows around (and out of the conversation list, since GetMessagesByUser
// filters them) so the user can recover from vault if needed.
func (s *Server) HandleDesktopDeleteMessages(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req struct {
		Messages []struct {
			Folder string `json:"folder"`
			UID    int64  `json:"uid"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	deleted := 0
	queued := 0
	for _, ref := range req.Messages {
		msg, err := s.database.GetMessageByID(ref.UID)
		if err != nil || msg.UserID != user.ID {
			continue
		}
		if err := s.database.SoftDeleteMessage(ref.UID); err != nil {
			continue
		}
		deleted++

		// External-account messages need the delete proxied to the source
		// IMAP server — otherwise the message stays in the user's "real"
		// inbox on small.kz / yandex / etc. forever. We queue a flag-sync
		// entry with deleted=true; the worker stores \Deleted + UID
		// EXPUNGE on the remote folder.
		//
		// account_id = 0 means a locally-delivered message (our own MX);
		// remote_uid = 0 means we never learned the source UID (e.g. the
		// message predates the remote_uid tracking migration). In both
		// cases there's nothing to push.
		if msg.AccountID > 0 && msg.RemoteUID > 0 {
			if err := s.database.QueueFlagSync(
				msg.ID, msg.AccountID, msg.RemoteFolder, msg.RemoteUID,
				msg.Seen, msg.Flagged, msg.Answered, true,
			); err != nil {
				log.Printf("desktop delete: queue flag sync failed for msg %d: %v", msg.ID, err)
			} else {
				queued++
			}
		}
	}

	log.Printf("desktop delete: soft-deleted %d/%d messages, queued %d for remote sync", deleted, len(req.Messages), queued)
	respondJSON(w, http.StatusOK, map[string]int{"deleted": deleted, "queued_remote": queued})
}

// ── Identities ──

// HandleDesktopIdentities returns the user's email identities.
func (s *Server) HandleDesktopIdentities(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	type Identity struct {
		Email     string `json:"email"`
		Name      string `json:"name"`
		Signature string `json:"signature"`
		IsDefault bool   `json:"is_default"`
	}

	identities := []Identity{}

	// Local mailboxes
	mailboxes, err := s.database.GetMailboxesWithDomainByUserID(user.ID)
	if err == nil {
		for i, mb := range mailboxes {
			if !mb.Enabled {
				continue
			}
			identities = append(identities, Identity{
				Email:     fmt.Sprintf("%s@%s", mb.LocalPart, mb.DomainName),
				Name:      user.Username,
				IsDefault: i == 0,
			})
		}
	}

	// External accounts + aliases
	accounts, err := s.database.GetAccountsByUserID(user.ID)
	if err == nil {
		for _, acc := range accounts {
			if acc.Email == "" || !acc.Enabled {
				continue
			}
			identities = append(identities, Identity{
				Email:     acc.Email,
				Name:      acc.Name,
				IsDefault: len(identities) == 0,
			})
			for _, alias := range acc.GetAliases() {
				identities = append(identities, Identity{
					Email: alias,
					Name:  acc.Name,
				})
			}
		}
	}

	if len(identities) == 0 {
		identities = append(identities, Identity{
			Email:     user.Username,
			Name:      user.Username,
			IsDefault: true,
		})
	}

	respondJSON(w, http.StatusOK, identities)
}

// ── Send ──

// HandleDesktopSend queues a message for sending.
func (s *Server) HandleDesktopSend(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req struct {
		From        string   `json:"from"`
		To          []string `json:"to"`
		Cc          []string `json:"cc"`
		Subject     string   `json:"subject"`
		HTML        string   `json:"html"`
		Text        string   `json:"text"`
		InReplyTo   string   `json:"in_reply_to"`
		References  string   `json:"references"`
		Attachments []struct {
			Filename  string  `json:"filename"`
			MimeType  string  `json:"mime_type"`
			Content   []byte  `json:"content"`
			ContentID *string `json:"content_id"`
		} `json:"attachments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Determine account
	accountID := int64(0)
	accounts, err := s.database.GetAccountsByUserID(user.ID)
	if err == nil {
		fromEmail := extractEmail(req.From)
		for _, acc := range accounts {
			if strings.EqualFold(acc.Email, fromEmail) {
				accountID = acc.ID
				break
			}
		}
	}

	outboxMsg := &models.OutboxMessage{
		UserID:    user.ID,
		AccountID: accountID,
		From:      req.From,
		To:        strings.Join(req.To, ", "),
		Cc:        strings.Join(req.Cc, ", "),
		Subject:   req.Subject,
		Body:      req.Text,
		BodyHTML:  req.HTML,
		Status:    "pending",
	}

	var rawAtts []emailAttachment
	for _, a := range req.Attachments {
		cid := ""
		if a.ContentID != nil {
			cid = *a.ContentID
		}
		rawAtts = append(rawAtts, emailAttachment{
			Filename:  a.Filename,
			MimeType:  a.MimeType,
			Data:      a.Content,
			ContentID: cid,
		})
	}

	// If threading headers or attachments are present, build full RawEmail.
	// send_task sends RawEmail verbatim when non-empty (smtp/client/send_task.go:80),
	// so this preserves both threading and attachments.
	hasThreading := req.InReplyTo != "" || req.References != ""
	hasAttachments := len(rawAtts) > 0
	if hasThreading || hasAttachments {
		outboxMsg.RawEmail = buildRawEmail(
			req.From, outboxMsg.To, outboxMsg.Cc, req.Subject, req.Text, req.HTML,
			req.InReplyTo, req.References, rawAtts,
		)
	}

	if err := s.database.CreateOutboxMessage(outboxMsg); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to queue message")
		return
	}

	// Also save attachments to outbox_attachments table (for fallback path
	// when constructEmail is used instead of RawEmail).
	for _, a := range rawAtts {
		s.database.CreateOutboxAttachment(&models.OutboxAttachment{
			OutboxMessageID: outboxMsg.ID,
			Filename:        a.Filename,
			ContentType:     a.MimeType,
			Size:            len(a.Data),
			Data:            a.Data,
			ContentID:       a.ContentID,
		})
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":     "queued",
		"message_id": outboxMsg.ID,
	})
}

// ── Conversations ──

// collectIdentities returns lowercase email set for the user (local mailboxes + accounts + aliases).
func (s *Server) collectIdentities(userID int64) map[string]bool {
	ids := make(map[string]bool)

	mailboxes, err := s.database.GetMailboxesWithDomainByUserID(userID)
	if err == nil {
		for _, mb := range mailboxes {
			if mb.Enabled {
				ids[strings.ToLower(fmt.Sprintf("%s@%s", mb.LocalPart, mb.DomainName))] = true
			}
		}
	}

	accounts, err := s.database.GetAccountsByUserID(userID)
	if err == nil {
		for _, acc := range accounts {
			if acc.Email != "" && acc.Enabled {
				ids[strings.ToLower(acc.Email)] = true
				for _, alias := range acc.GetAliases() {
					ids[strings.ToLower(alias)] = true
				}
			}
		}
	}

	return ids
}

func gravatarHash(email string) string {
	h := md5.Sum([]byte(strings.TrimSpace(strings.ToLower(email))))
	return hex.EncodeToString(h[:])
}

// DesktopContactInfo matches the client's ContactInfo type.
type DesktopContactInfo struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
}

// DesktopMessageRef matches the client's MessageRef type.
type DesktopMessageRef struct {
	Folder string `json:"folder"`
	UID    int64  `json:"uid"` // messages.id in native mode
}

// DesktopConversation matches the client's Conversation type.
type DesktopConversation struct {
	ID           string               `json:"id"`
	Label        string               `json:"label"`
	AvatarHash   string               `json:"avatar_hash"`
	ReceivedBy   string               `json:"received_by"`
	Counterparts []DesktopContactInfo `json:"counterparts"`
	IsGroup      bool                 `json:"is_group"`
	LastDate     string               `json:"last_date"`
	LastDateTS   int64                `json:"last_date_ts"`
	LastSubject  string               `json:"last_subject"`
	UnreadCount  int                  `json:"unread_count"`
	TotalCount   int                  `json:"total_count"`
	Messages     []DesktopMessageRef  `json:"messages"`
	Draft        *DesktopMessageRef   `json:"draft"`
}

type msgEntry struct {
	msg        *models.Message
	folderName string
}

// HandleDesktopConversations groups messages into conversations.
func (s *Server) HandleDesktopConversations(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// Per-folder cap. Used as the LIMIT in GetMessagesByFolder before we
	// group messages into conversations — so if the cap is too low we
	// silently lose entire conversations whose latest message is older
	// than the top-N. With users hitting ~2-3k messages in INBOX after
	// pulling all non-Trash folders, the old default of 200 dropped
	// most of February-April off the chat list. Bumped to 5000 with a
	// 20000 ceiling; query overhead at this size is ms-not-seconds and
	// the grouping step deduplicates so the wire payload stays bounded
	// by unique conversations, not raw message count.
	limit := 5000
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 20000 {
		limit = n
	}

	identities := s.collectIdentities(user.ID)
	isOurs := func(addr string) bool { return identities[strings.ToLower(addr)] }

	// Per-folder fetch mirroring the IMAP fallback path: pull recent rows from
	// INBOX/Sent/Drafts the user is subscribed to. Trash/Spam/Archive stay
	// invisible in the chat list, and an unsubscribed-from inbox of a second
	// account doesn't leak in either. Per-folder limits keep this O(limit)
	// regardless of total mailbox size, which matters once user inboxes grow
	// into the tens of thousands.
	folders, _ := s.database.GetFoldersByUser(user.ID)
	subscribed, _ := s.database.GetSubscribedFolderIDs(user.ID)
	folderNames := make(map[int64]string)
	folderAccount := make(map[int64]int64)
	sentFolderIDs := make(map[int64]bool)
	for _, f := range folders {
		folderNames[f.ID] = f.Name
		folderAccount[f.ID] = f.AccountID
		if f.Type == "sent" {
			sentFolderIDs[f.ID] = true
		}
	}

	// account_id → primary email, plus a sorted identity list. Both used by
	// the "which identity received this message" fallback below: To/Cc parsing
	// is the primary signal but breaks for BCC-only deliveries, stripped
	// headers, and (most visibly) for messages restored from spam where the
	// original recipient header may not match any identity. We fall through
	// to the message's account email, then the folder's, then a stable sort
	// — never a random map iteration.
	accountEmail := make(map[int64]string)
	if accounts, err := s.database.GetAccountsByUserID(user.ID); err == nil {
		for _, acc := range accounts {
			if acc.Email != "" {
				accountEmail[acc.ID] = strings.ToLower(acc.Email)
			}
		}
	}
	sortedIdentities := make([]string, 0, len(identities))
	for id := range identities {
		sortedIdentities = append(sortedIdentities, id)
	}
	sort.Strings(sortedIdentities)

	type folderQuota struct {
		id    int64
		quota int
	}
	quotas := []folderQuota{}
	for _, f := range folders {
		if !subscribed[f.ID] {
			continue
		}
		switch f.Type {
		case "inbox":
			quotas = append(quotas, folderQuota{f.ID, limit})
		case "sent":
			quotas = append(quotas, folderQuota{f.ID, limit / 2})
		case "drafts":
			quotas = append(quotas, folderQuota{f.ID, limit / 4})
		}
	}

	messages := []*models.Message{}
	for _, q := range quotas {
		if q.quota <= 0 {
			continue
		}
		batch, err := s.database.GetMessagesByFolder(q.id, q.quota, 0)
		if err != nil {
			log.Printf("HandleDesktopConversations: GetMessagesByFolder %d: %v", q.id, err)
			continue
		}
		// GetMessagesByFolder doesn't filter by user — guard at the call site.
		for _, m := range batch {
			if m.UserID == user.ID {
				messages = append(messages, m)
			}
		}
	}

	// Group by (my_id, sorted_counterparts). For 1:1 messages this collapses to
	// the historical (my_id, single_cp) key — preserving the "{my_id}|{cp}" id
	// format that pinned-conversation localStorage and search-result handlers
	// depend on. For multi-recipient mail (To+Cc has >1 non-self address, or
	// the sender is not among our identities and other recipients exist) we
	// keep all counterparts in the key so each unique participant set forms its
	// own group conversation.
	type convKey struct {
		myID string
		cps  string // comma-joined sorted unique counterpart addresses
	}
	type convMeta struct {
		entries []msgEntry
		cpAddrs []string // canonical sorted counterpart list for this key
	}
	convMap := make(map[convKey]*convMeta)

	for _, msg := range messages {
		fromLc := strings.ToLower(extractEmail(msg.From))
		fname := folderNames[msg.FolderID]

		recipients := parseRecipientAddrs(msg.To, msg.Cc)

		// Pick myID: for outgoing, the sender; for incoming, the first of our
		// addresses present in To/Cc.
		var myID string
		if isOurs(fromLc) {
			myID = fromLc
		} else {
			for _, r := range recipients {
				if isOurs(r) {
					myID = r
					break
				}
			}
			if myID == "" {
				// Recipient parsing missed (BCC-only, stripped header, etc.).
				// Walk a priority chain that never returns a random identity.
				if e, ok := accountEmail[msg.AccountID]; ok && identities[e] {
					myID = e
				}
			}
			if myID == "" {
				if accID, ok := folderAccount[msg.FolderID]; ok {
					if e, ok := accountEmail[accID]; ok && identities[e] {
						myID = e
					}
				}
			}
			if myID == "" && len(sortedIdentities) > 0 {
				// Deterministic last resort — picks the alphabetically first
				// identity. Still wrong sometimes, but at least stable across
				// reloads and across users with the same identity set.
				myID = sortedIdentities[0]
			}
		}

		// Counterparts = union of (from, to, cc) minus our identities, deduped
		// and sorted for canonical key form. Skip pure self-traffic.
		seen := map[string]bool{}
		cps := []string{}
		add := func(a string) {
			if a == "" || isOurs(a) || seen[a] {
				return
			}
			seen[a] = true
			cps = append(cps, a)
		}
		add(fromLc)
		for _, r := range recipients {
			add(r)
		}
		if len(cps) == 0 {
			continue
		}
		sort.Strings(cps)

		key := convKey{myID: myID, cps: strings.Join(cps, ",")}
		meta, ok := convMap[key]
		if !ok {
			meta = &convMeta{cpAddrs: cps}
			convMap[key] = meta
		}
		meta.entries = append(meta.entries, msgEntry{msg: msg, folderName: fname})
	}

	// Build conversation objects
	convs := []DesktopConversation{}

	for key, meta := range convMap {
		entries := meta.entries
		// Sort by date ascending
		sortEntriesByDate(entries)

		// Dedup by message_id (prefer Sent copy)
		entries = dedupEntries(entries, sentFolderIDs)

		// Separate drafts
		regular := []msgEntry{}
		var lastDraft *msgEntry
		for i := range entries {
			if entries[i].msg.Draft {
				lastDraft = &entries[i]
			} else {
				regular = append(regular, entries[i])
			}
		}

		// Drafts never start a conversation on their own. They're useful only
		// to resume composition for a thread that already exists (regular
		// messages present). A draft-only conv would render with empty
		// messages[] and pollute the sidebar with what's effectively a
		// scratch buffer.
		if len(regular) == 0 {
			continue
		}

		// Stats from regular messages
		unread := 0
		var firstMsg, lastMsg *msgEntry
		for i := range regular {
			if !regular[i].msg.Seen {
				unread++
			}
			if firstMsg == nil {
				firstMsg = &regular[i]
			}
			lastMsg = &regular[i]
		}
		if lastMsg == nil && lastDraft != nil {
			lastMsg = lastDraft
		}
		if firstMsg == nil {
			firstMsg = lastMsg
		}

		// Resolve display names for each counterpart from any message in the
		// thread that referenced them (From for incoming, To/Cc names for
		// outgoing). First match wins. Anything that looks like a raw address
		// is dropped — those are useless as labels.
		cpInfos := make([]DesktopContactInfo, 0, len(meta.cpAddrs))
		for _, addr := range meta.cpAddrs {
			cpInfos = append(cpInfos, DesktopContactInfo{Addr: addr, Name: nameForAddr(entries, addr)})
		}

		isGroup := len(meta.cpAddrs) > 1

		// 1:1: label = counterpart name (or addr).
		// Group: label = first message subject + " (N чел)" where N counts all
		// human participants (counterparts + me).
		var label string
		var id string
		if isGroup {
			subject := strings.TrimSpace(firstMsg.msg.Subject)
			if subject == "" {
				subject = "(без темы)"
			}
			label = fmt.Sprintf("%s (%d чел)", subject, len(meta.cpAddrs)+1)
			id = fmt.Sprintf("%s|group:%s", key.myID, key.cps)
		} else {
			label = cpInfos[0].Name
			if label == "" {
				label = cpInfos[0].Addr
			}
			id = fmt.Sprintf("%s|%s", key.myID, key.cps)
		}

		// Build message refs
		msgRefs := make([]DesktopMessageRef, 0, len(regular))
		for _, e := range regular {
			msgRefs = append(msgRefs, DesktopMessageRef{
				Folder: e.folderName,
				UID:    e.msg.ID,
			})
		}

		var draftRef *DesktopMessageRef
		if lastDraft != nil {
			draftRef = &DesktopMessageRef{
				Folder: lastDraft.folderName,
				UID:    lastDraft.msg.ID,
			}
		}

		// Avatar: use the first counterpart for both 1:1 and groups (groups
		// would ideally show a stack but that's UI-side).
		avatarSeed := cpInfos[0].Addr

		conv := DesktopConversation{
			ID:           id,
			Label:        label,
			AvatarHash:   gravatarHash(avatarSeed),
			ReceivedBy:   key.myID,
			Counterparts: cpInfos,
			IsGroup:      isGroup,
			LastDate:     timeutil.FromMs(lastMsg.msg.Date).Format(time.RFC1123Z),
			LastDateTS:   lastMsg.msg.Date / 1000,
			LastSubject:  lastMsg.msg.Subject,
			UnreadCount:  unread,
			TotalCount:   len(regular),
			Messages:     msgRefs,
			Draft:        draftRef,
		}
		convs = append(convs, conv)
	}

	// Sort by last_date_ts DESC and limit
	sortConversationsByDate(convs)
	if len(convs) > limit {
		convs = convs[:limit]
	}

	respondJSON(w, http.StatusOK, convs)
}

// parseRecipientAddrs extracts lowercased email addresses from To and Cc fields.
func parseRecipientAddrs(to, cc string) []string {
	var result []string
	for _, field := range []string{to, cc} {
		for _, part := range strings.Split(field, ",") {
			addr := strings.ToLower(extractEmail(strings.TrimSpace(part)))
			if addr != "" && strings.Contains(addr, "@") {
				result = append(result, addr)
			}
		}
	}
	return result
}

// nameForAddr looks across thread messages for a display name attached to a
// specific address (in From or in any To/Cc entry). Returns the first match
// that isn't itself an email-looking string; empty string if none found.
func nameForAddr(entries []msgEntry, addr string) string {
	addrLc := strings.ToLower(addr)
	for _, e := range entries {
		// Check From
		if strings.ToLower(extractEmail(e.msg.From)) == addrLc {
			if n := extractName(e.msg.From); n != "" && !strings.Contains(n, "@") {
				return n
			}
		}
		// Check each "Name <email>" in To and Cc
		for _, field := range []string{e.msg.To, e.msg.Cc} {
			for _, part := range strings.Split(field, ",") {
				part = strings.TrimSpace(part)
				if part == "" {
					continue
				}
				if strings.ToLower(extractEmail(part)) == addrLc {
					if n := extractName(part); n != "" && !strings.Contains(n, "@") {
						return n
					}
				}
			}
		}
	}
	return ""
}

// extractName extracts display name from "Name <email>" format.
func extractName(addr string) string {
	addr = strings.TrimSpace(addr)
	if idx := strings.Index(addr, "<"); idx > 0 {
		name := strings.TrimSpace(addr[:idx])
		name = strings.Trim(name, "\"'")
		return name
	}
	return ""
}

func sortEntriesByDate(entries []msgEntry) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].msg.Date < entries[j-1].msg.Date; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}

func dedupEntries(entries []msgEntry, sentFolderIDs map[int64]bool) []msgEntry {
	seen := make(map[string]int)
	keep := make([]bool, len(entries))
	for i := range keep {
		keep[i] = true
	}

	for i, e := range entries {
		mid := e.msg.MessageID
		if mid == "" {
			continue
		}
		if j, exists := seen[mid]; exists {
			isSent := sentFolderIDs[e.msg.FolderID]
			jIsSent := sentFolderIDs[entries[j].msg.FolderID]
			if isSent && !jIsSent {
				keep[j] = false
				seen[mid] = i
			} else {
				keep[i] = false
			}
		} else {
			seen[mid] = i
		}
	}

	var result []msgEntry
	for i, e := range entries {
		if keep[i] {
			result = append(result, e)
		}
	}
	return result
}

func sortConversationsByDate(convs []DesktopConversation) {
	for i := 1; i < len(convs); i++ {
		for j := i; j > 0 && convs[j].LastDateTS > convs[j-1].LastDateTS; j-- {
			convs[j], convs[j-1] = convs[j-1], convs[j]
		}
	}
}

// ── Conversation messages ──

// DesktopMessageBody matches the client's MessageBody type.
type DesktopMessageBody struct {
	UID         int64               `json:"uid"` // messages.id
	Folder      string              `json:"folder"`
	Subject     string              `json:"subject"`
	From        string              `json:"from"`
	FromAddr    string              `json:"from_addr"`
	To          []string            `json:"to"`
	Cc          []string            `json:"cc"`
	Date        string              `json:"date"`
	DateTS      int64               `json:"date_ts"`
	HTML        *string             `json:"html"`
	Text        *string             `json:"text"`
	Attachments []DesktopAttachment `json:"attachments"`
	IsOutgoing  bool                `json:"is_outgoing"`
	MessageID   string              `json:"message_id"`
	InReplyTo   string              `json:"in_reply_to"`
	References  []string            `json:"references"`
}

type DesktopAttachment struct {
	Filename string `json:"filename"`
	MimeType string `json:"mime_type"`
	Size     int    `json:"size"`
	Index    int    `json:"index"`
}

// HandleDesktopConversationMessages returns full message bodies for given refs.
func (s *Server) HandleDesktopConversationMessages(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var refs []DesktopMessageRef
	if err := json.NewDecoder(r.Body).Decode(&refs); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	identities := s.collectIdentities(user.ID)

	// Build folder ID→name map
	folders, _ := s.database.GetFoldersByUser(user.ID)
	folderNames := make(map[int64]string)
	for _, f := range folders {
		folderNames[f.ID] = f.Name
	}

	bodies := []DesktopMessageBody{}
	for _, ref := range refs {
		msg, err := s.database.GetMessageByID(ref.UID)
		if err != nil || msg.UserID != user.ID {
			continue
		}

		fromAddr := strings.ToLower(extractEmail(msg.From))
		isOutgoing := identities[fromAddr]

		// Parse To/Cc into string slices
		toList := splitAndTrim(msg.To)
		ccList := splitAndTrim(msg.Cc)

		// Get attachments
		atts := []DesktopAttachment{}
		dbAtts, err := s.database.GetAttachmentsByMessageID(msg.ID)
		if err == nil {
			for i, a := range dbAtts {
				if a.IsInline {
					continue
				}
				atts = append(atts, DesktopAttachment{
					Filename: parser.DecodeMIMEHeader(a.Filename),
					MimeType: a.ContentType,
					Size:     a.Size,
					Index:    i,
				})
			}
		}

		// Parse references
		msgRefs := []string{}
		if msg.MessageReferences != "" {
			msgRefs = parseMessageIDs(msg.MessageReferences)
		}

		var html, text *string
		if msg.BodyHTML != "" {
			html = &msg.BodyHTML
		}
		if msg.Body != "" {
			text = &msg.Body
		}

		bodies = append(bodies, DesktopMessageBody{
			UID:         msg.ID,
			Folder:      folderNames[msg.FolderID],
			Subject:     msg.Subject,
			From:        msg.From,
			FromAddr:    fromAddr,
			To:          toList,
			Cc:          ccList,
			Date:        timeutil.FromMs(msg.Date).Format(time.RFC1123Z),
			DateTS:      msg.Date / 1000,
			HTML:        html,
			Text:        text,
			Attachments: atts,
			IsOutgoing:  isOutgoing,
			MessageID:   msg.MessageID,
			InReplyTo:   msg.InReplyTo,
			References:  msgRefs,
		})
	}

	// Sort by date ascending (oldest first, like chat view)
	for i := 1; i < len(bodies); i++ {
		for j := i; j > 0 && bodies[j].DateTS < bodies[j-1].DateTS; j-- {
			bodies[j], bodies[j-1] = bodies[j-1], bodies[j]
		}
	}

	respondJSON(w, http.StatusOK, bodies)
}

func splitAndTrim(s string) []string {
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	result := []string{}
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// parseMessageIDs extracts Message-IDs from a References header value.
func parseMessageIDs(refs string) []string {
	var result []string
	var current strings.Builder
	depth := 0
	for _, ch := range refs {
		switch ch {
		case '<':
			depth++
			current.Reset()
		case '>':
			if depth > 0 {
				depth--
				id := strings.TrimSpace(current.String())
				if id != "" {
					result = append(result, id)
				}
				current.Reset()
			}
		default:
			if depth > 0 {
				current.WriteRune(ch)
			}
		}
	}
	return result
}

// extractEmail extracts email from "Name <email>" format.
func extractEmail(addr string) string {
	addr = strings.TrimSpace(addr)
	if start := strings.Index(addr, "<"); start >= 0 {
		if end := strings.Index(addr[start:], ">"); end > 0 {
			return strings.TrimSpace(addr[start+1 : start+end])
		}
	}
	return addr
}

// setUserContext puts user into context using the shared key.
func setUserContext(ctx context.Context, user *models.User) context.Context {
	return context.WithValue(ctx, userContextKey, user)
}

// emailAttachment is a raw attachment for MIME building.
type emailAttachment struct {
	Filename  string
	MimeType  string
	Data      []byte
	ContentID string // empty = file attachment, non-empty = inline image
}

// buildRawEmail produces a full RFC 5322 message with optional threading
// headers and attachments (both inline and file). MIME structure:
//   - body_alt = multipart/alternative { text/plain, text/html }
//   - if inline images → wrap body_alt in multipart/related, add inline parts
//   - if file attachments → wrap everything in multipart/mixed, add file parts
func buildRawEmail(from, to, cc, subject, text, html, inReplyTo, references string, atts []emailAttachment) []byte {
	domain := "localhost"
	if at := strings.LastIndex(from, "@"); at >= 0 {
		d := from[at+1:]
		d = strings.TrimRight(d, ">")
		d = strings.TrimSpace(d)
		if d != "" {
			domain = d
		}
	}
	idBytes := make([]byte, 8)
	_, _ = rand.Read(idBytes)
	messageID := fmt.Sprintf("<%d.%s@%s>", time.Now().UnixNano(), hex.EncodeToString(idBytes), domain)

	var b strings.Builder

	// Headers
	fmt.Fprintf(&b, "Message-ID: %s\r\n", messageID)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format("Mon, 02 Jan 2006 15:04:05 -0700"))
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	if cc != "" {
		fmt.Fprintf(&b, "Cc: %s\r\n", cc)
	}
	if subject != "" {
		fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	}
	if inReplyTo != "" {
		fmt.Fprintf(&b, "In-Reply-To: %s\r\n", inReplyTo)
	}
	if references != "" {
		fmt.Fprintf(&b, "References: %s\r\n", references)
	}
	b.WriteString("MIME-Version: 1.0\r\n")

	// Separate inline images from file attachments
	var inlineAtts, fileAtts []emailAttachment
	for _, a := range atts {
		if a.ContentID != "" {
			inlineAtts = append(inlineAtts, a)
		} else {
			fileAtts = append(fileAtts, a)
		}
	}

	// Build body_alt (text/html alternative)
	altBoundary := fmt.Sprintf("----=_Alt_%s", hex.EncodeToString(idBytes))
	hasText, hasHTML := text != "", html != ""

	writeBodyAlt := func(w *strings.Builder) {
		switch {
		case hasText && hasHTML:
			fmt.Fprintf(w, "Content-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", altBoundary)
			fmt.Fprintf(w, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", altBoundary, text)
			fmt.Fprintf(w, "--%s\r\nContent-Type: text/html; charset=utf-8\r\n\r\n%s\r\n", altBoundary, html)
			fmt.Fprintf(w, "--%s--\r\n", altBoundary)
		case hasHTML:
			w.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
			w.WriteString(html)
			w.WriteString("\r\n")
		default:
			w.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
			w.WriteString(text)
			w.WriteString("\r\n")
		}
	}

	writeBase64Part := func(w *strings.Builder, boundary string, a emailAttachment) {
		fmt.Fprintf(w, "--%s\r\n", boundary)
		fmt.Fprintf(w, "Content-Type: %s; name=\"%s\"\r\n", a.MimeType, a.Filename)
		w.WriteString("Content-Transfer-Encoding: base64\r\n")
		if a.ContentID != "" {
			fmt.Fprintf(w, "Content-ID: <%s>\r\n", a.ContentID)
			fmt.Fprintf(w, "Content-Disposition: inline; filename=\"%s\"\r\n\r\n", a.Filename)
		} else {
			fmt.Fprintf(w, "Content-Disposition: attachment; filename=\"%s\"\r\n\r\n", a.Filename)
		}
		encoded := base64.StdEncoding.EncodeToString(a.Data)
		for i := 0; i < len(encoded); i += 76 {
			end := i + 76
			if end > len(encoded) {
				end = len(encoded)
			}
			w.WriteString(encoded[i:end])
			w.WriteString("\r\n")
		}
	}

	// No attachments at all — simple body
	if len(inlineAtts) == 0 && len(fileAtts) == 0 {
		writeBodyAlt(&b)
		return []byte(b.String())
	}

	// Has inline images → wrap body in multipart/related
	relBoundary := fmt.Sprintf("----=_Rel_%s", hex.EncodeToString(idBytes))
	mixBoundary := fmt.Sprintf("----=_Mix_%s", hex.EncodeToString(idBytes))

	if len(fileAtts) > 0 {
		// Outermost: multipart/mixed
		fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=\"%s\"\r\n\r\n", mixBoundary)

		if len(inlineAtts) > 0 {
			// Body + inlines wrapped in multipart/related
			fmt.Fprintf(&b, "--%s\r\n", mixBoundary)
			fmt.Fprintf(&b, "Content-Type: multipart/related; boundary=\"%s\"\r\n\r\n", relBoundary)
			fmt.Fprintf(&b, "--%s\r\n", relBoundary)
			writeBodyAlt(&b)
			for _, a := range inlineAtts {
				writeBase64Part(&b, relBoundary, a)
			}
			fmt.Fprintf(&b, "--%s--\r\n", relBoundary)
		} else {
			// No inlines — body directly inside mixed
			fmt.Fprintf(&b, "--%s\r\n", mixBoundary)
			writeBodyAlt(&b)
		}

		// File attachments
		for _, a := range fileAtts {
			writeBase64Part(&b, mixBoundary, a)
		}
		fmt.Fprintf(&b, "--%s--\r\n", mixBoundary)
	} else {
		// Only inline images, no file attachments → multipart/related at top
		fmt.Fprintf(&b, "Content-Type: multipart/related; boundary=\"%s\"\r\n\r\n", relBoundary)
		fmt.Fprintf(&b, "--%s\r\n", relBoundary)
		writeBodyAlt(&b)
		for _, a := range inlineAtts {
			writeBase64Part(&b, relBoundary, a)
		}
		fmt.Fprintf(&b, "--%s--\r\n", relBoundary)
	}

	return []byte(b.String())
}

// buildRawEmailWithThreading is a backward-compatible wrapper (no attachments).
func buildRawEmailWithThreading(from, to, cc, subject, text, html, inReplyTo, references string) []byte {
	return buildRawEmail(from, to, cc, subject, text, html, inReplyTo, references, nil)
}
