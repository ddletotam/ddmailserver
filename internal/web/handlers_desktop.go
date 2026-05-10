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
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/yourusername/mailserver/internal/models"
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

		switch req.Flags {
		case "\\Seen":
			s.database.UpdateMessageFlag(ref.UID, "seen", req.Add)
		case "\\Flagged":
			s.database.UpdateMessageFlag(ref.UID, "flagged", req.Add)
		case "\\Answered":
			s.database.UpdateMessageFlag(ref.UID, "answered", req.Add)
		case "\\Deleted":
			s.database.UpdateMessageFlag(ref.UID, "deleted", req.Add)
		case "\\Draft":
			s.database.UpdateMessageFlag(ref.UID, "draft", req.Add)
		}
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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

	limit := 200
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 1000 {
		limit = n
	}

	identities := s.collectIdentities(user.ID)
	isOurs := func(addr string) bool { return identities[strings.ToLower(addr)] }

	// Fetch messages across all folders (generous limit to cover grouping)
	messages, err := s.database.GetMessagesByUser(user.ID, limit*5, 0)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get messages")
		return
	}

	// Build folder ID→name map
	folders, _ := s.database.GetFoldersByUser(user.ID)
	folderNames := make(map[int64]string)
	sentFolderIDs := make(map[int64]bool)
	for _, f := range folders {
		folderNames[f.ID] = f.Name
		if f.Type == "sent" {
			sentFolderIDs[f.ID] = true
		}
	}

	// Group by (my_id, counterpart)
	type convKey struct{ myID, cp string }
	convMap := make(map[convKey][]msgEntry)

	for _, msg := range messages {
		fromLc := strings.ToLower(extractEmail(msg.From))
		fname := folderNames[msg.FolderID]

		var myID, cp string
		if isOurs(fromLc) {
			// Outgoing: counterpart = first non-self in to+cc
			recipients := parseRecipientAddrs(msg.To, msg.Cc)
			cp = ""
			for _, r := range recipients {
				if !isOurs(r) {
					cp = r
					break
				}
			}
			if cp == "" {
				// All recipients are ours — pick first that differs from sender
				for _, r := range recipients {
					if r != fromLc {
						cp = r
						break
					}
				}
			}
			if cp == "" {
				continue
			}
			myID = fromLc
		} else {
			// Incoming: my_id = first of our addrs in to+cc
			recipients := parseRecipientAddrs(msg.To, msg.Cc)
			myID = ""
			for _, r := range recipients {
				if isOurs(r) {
					myID = r
					break
				}
			}
			if myID == "" {
				// Fallback: use first identity
				for id := range identities {
					myID = id
					break
				}
			}
			cp = fromLc
		}

		key := convKey{myID: myID, cp: cp}
		convMap[key] = append(convMap[key], msgEntry{msg: msg, folderName: fname})
	}

	// Build conversation objects
	convs := []DesktopConversation{}

	for key, entries := range convMap {
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

		if len(regular) == 0 && lastDraft == nil {
			continue
		}

		// Stats from regular messages
		unread := 0
		var lastMsg *msgEntry
		for i := range regular {
			if !regular[i].msg.Seen {
				unread++
			}
			lastMsg = &regular[i]
		}
		if lastMsg == nil && lastDraft != nil {
			lastMsg = lastDraft
		}

		// Display name for counterpart
		cpName := ""
		for _, e := range entries {
			fromLc := strings.ToLower(extractEmail(e.msg.From))
			if fromLc == key.cp {
				name := extractName(e.msg.From)
				if name != "" && !strings.Contains(name, "@") {
					cpName = name
					break
				}
			}
		}
		label := cpName
		if label == "" {
			label = key.cp
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

		conv := DesktopConversation{
			ID:         fmt.Sprintf("%s|%s", key.myID, key.cp),
			Label:      label,
			AvatarHash: gravatarHash(key.cp),
			ReceivedBy: key.myID,
			Counterparts: []DesktopContactInfo{{
				Name: cpName,
				Addr: key.cp,
			}},
			IsGroup:     false,
			LastDate:    timeutil.FromMs(lastMsg.msg.Date).Format(time.RFC1123Z),
			LastDateTS:  lastMsg.msg.Date / 1000,
			LastSubject: lastMsg.msg.Subject,
			UnreadCount: unread,
			TotalCount:  len(regular),
			Messages:    msgRefs,
			Draft:       draftRef,
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
					Filename: a.Filename,
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
