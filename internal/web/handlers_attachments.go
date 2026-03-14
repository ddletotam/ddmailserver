package web

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"

	"github.com/gorilla/mux"
)

// cidRegex matches cid: URLs in HTML (src="cid:xxx" or src='cid:xxx')
var cidRegex = regexp.MustCompile(`(src\s*=\s*["'])cid:([^"']+)(["'])`)

// replaceCIDURLs replaces cid: URLs with actual attachment URLs
func replaceCIDURLs(html string, messageID int64) string {
	return cidRegex.ReplaceAllStringFunc(html, func(match string) string {
		// Extract parts: src="cid:xxx"
		parts := cidRegex.FindStringSubmatch(match)
		if len(parts) != 4 {
			return match
		}
		prefix := parts[1] // src="
		cid := parts[2]    // content-id
		suffix := parts[3] // "

		// Build new URL
		newURL := fmt.Sprintf("/messages/%d/attachments/cid/%s", messageID, cid)
		return prefix + newURL + suffix
	})
}

// HandleAttachment serves an attachment by ID
func (s *Server) HandleAttachment(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	id, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid attachment ID", http.StatusBadRequest)
		return
	}

	// Get attachment
	attachment, err := s.database.GetAttachmentByID(id)
	if err != nil {
		http.Error(w, "Attachment not found", http.StatusNotFound)
		return
	}

	// Verify user owns this attachment (via message)
	message, err := s.database.GetMessageByID(attachment.MessageID)
	if err != nil || message.UserID != user.ID {
		http.Error(w, "Attachment not found", http.StatusNotFound)
		return
	}

	// Set headers
	w.Header().Set("Content-Type", attachment.ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(attachment.Size))

	if !attachment.IsInline {
		w.Header().Set("Content-Disposition", "attachment; filename=\""+attachment.Filename+"\"")
	}

	w.Write(attachment.Data)
}

// HandleAttachmentByCID serves an inline attachment by Content-ID
func (s *Server) HandleAttachmentByCID(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	messageID, err := strconv.ParseInt(vars["messageId"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid message ID", http.StatusBadRequest)
		return
	}

	cid := vars["cid"]
	if cid == "" {
		http.Error(w, "Content-ID required", http.StatusBadRequest)
		return
	}

	// Verify user owns this message
	message, err := s.database.GetMessageByID(messageID)
	if err != nil || message.UserID != user.ID {
		http.Error(w, "Message not found", http.StatusNotFound)
		return
	}

	// Get attachment by CID
	attachment, err := s.database.GetAttachmentByContentID(messageID, cid)
	if err != nil || attachment == nil {
		http.Error(w, "Attachment not found", http.StatusNotFound)
		return
	}

	// Set headers
	w.Header().Set("Content-Type", attachment.ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(attachment.Size))
	w.Header().Set("Cache-Control", "public, max-age=31536000") // Cache for 1 year

	w.Write(attachment.Data)
}
