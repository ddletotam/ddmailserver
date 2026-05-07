package web

import (
	"context"
	"crypto/rand"
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

	var result []DesktopFolder
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

	// Reconstruct minimal RFC-822 from stored fields
	w.Header().Set("Content-Type", "message/rfc822")
	fmt.Fprintf(w, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: %s\r\n\r\n%s",
		msg.From, msg.To, msg.Subject, msg.Date.Format("Mon, 02 Jan 2006 15:04:05 -0700"),
		msg.MessageID, msg.Body)
}

// HandleDesktopSetFlags updates flags on messages.
func (s *Server) HandleDesktopSetFlags(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req struct {
		MessageIDs []int64 `json:"message_ids"`
		Flags      string  `json:"flags"` // e.g. "\\Seen"
		Add        bool    `json:"add"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	for _, msgID := range req.MessageIDs {
		msg, err := s.database.GetMessageByID(msgID)
		if err != nil || msg.UserID != user.ID {
			continue
		}

		switch req.Flags {
		case "\\Seen":
			s.database.UpdateMessageFlag(msgID, "seen", req.Add)
		case "\\Flagged":
			s.database.UpdateMessageFlag(msgID, "flagged", req.Add)
		case "\\Answered":
			s.database.UpdateMessageFlag(msgID, "answered", req.Add)
		case "\\Deleted":
			s.database.UpdateMessageFlag(msgID, "deleted", req.Add)
		case "\\Draft":
			s.database.UpdateMessageFlag(msgID, "draft", req.Add)
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

	var identities []Identity

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
		From       string   `json:"from"`
		To         []string `json:"to"`
		Cc         []string `json:"cc"`
		Subject    string   `json:"subject"`
		HTML       string   `json:"html"`
		Text       string   `json:"text"`
		InReplyTo  string   `json:"in_reply_to"`
		References string   `json:"references"`
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

	// Threading: outbox_messages has no in_reply_to/references columns and
	// constructEmail() in send_task ignores those fields, so a reply submitted
	// via the Desktop API would otherwise lose its threading headers. Pre-build
	// a RawEmail here when threading info is present — send_task uses RawEmail
	// verbatim when non-empty (smtp/client/send_task.go:80).
	if req.InReplyTo != "" || req.References != "" {
		outboxMsg.RawEmail = buildRawEmailWithThreading(
			req.From, outboxMsg.To, outboxMsg.Cc, req.Subject, req.Text, req.HTML,
			req.InReplyTo, req.References,
		)
	}

	if err := s.database.CreateOutboxMessage(outboxMsg); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to queue message")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":     "queued",
		"message_id": outboxMsg.ID,
	})
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

// buildRawEmailWithThreading produces an RFC 5322 message including
// In-Reply-To/References headers. Used by HandleDesktopSend so that
// Desktop API replies preserve threading — without this, send_task's
// constructEmail() drops those headers (it has no field for them).
// Mirrors the structure of constructEmail() for the no-attachments
// case (Desktop API does not yet accept attachments).
func buildRawEmailWithThreading(from, to, cc, subject, text, html, inReplyTo, references string) []byte {
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

	hasText, hasHTML := text != "", html != ""
	switch {
	case hasText && hasHTML:
		boundary := fmt.Sprintf("----=_Part_%s", hex.EncodeToString(idBytes))
		fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", boundary)
		fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", boundary, text)
		fmt.Fprintf(&b, "--%s\r\nContent-Type: text/html; charset=utf-8\r\n\r\n%s\r\n", boundary, html)
		fmt.Fprintf(&b, "--%s--\r\n", boundary)
	case hasHTML:
		b.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
		b.WriteString(html)
	default:
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		b.WriteString(text)
	}
	return []byte(b.String())
}
