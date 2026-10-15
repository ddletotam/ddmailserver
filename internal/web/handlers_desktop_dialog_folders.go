package web

import (
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/ddletotam/ddmailserver/internal/notify"
)

// maxDialogFoldersDoc caps the stored document. Folders carry manual
// include/exclude lists of conversation ids (address sets), so a heavy user
// can reach tens of KB; a megabyte is far beyond any honest use.
const maxDialogFoldersDoc = 1 << 20

// dialogFoldersResponse is both the GET answer and the PUT answer.
// Doc is the client's own JSON, passed through untouched (null when the user
// never saved folders).
type dialogFoldersResponse struct {
	Version int64           `json:"version"`
	Doc     json.RawMessage `json:"doc"`
}

type dialogFoldersPut struct {
	BaseVersion int64           `json:"base_version"`
	Doc         json.RawMessage `json:"doc"`
}

// HandleDesktopDialogFolders returns the dialog-folders document (the tabs
// above the desktop conversation list). The server does not interpret it:
// conversations are computed by the client, so the folder rules are applied
// there too.
func (s *Server) HandleDesktopDialogFolders(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	doc, version, err := s.database.GetDialogFolders(user.ID)
	if err != nil {
		log.Printf("dialog folders: get user=%d: %v", user.ID, err)
		respondError(w, http.StatusInternalServerError, "failed to load dialog folders")
		return
	}
	resp := dialogFoldersResponse{Version: version, Doc: json.RawMessage("null")}
	if doc != "" {
		resp.Doc = json.RawMessage(doc)
	}
	respondJSON(w, http.StatusOK, resp)
}

// HandleDesktopDialogFoldersSave replaces the document if base_version is
// still current. A stale base answers 409 with the current version and
// document, so the client can reapply its change on top instead of
// silently overwriting another station's edit.
func (s *Server) HandleDesktopDialogFoldersSave(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxDialogFoldersDoc+4096))
	if err != nil {
		respondError(w, http.StatusBadRequest, "failed to read body")
		return
	}
	var req dialogFoldersPut
	if err := json.Unmarshal(body, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if len(req.Doc) == 0 || string(req.Doc) == "null" {
		respondError(w, http.StatusBadRequest, "doc is required")
		return
	}
	if len(req.Doc) > maxDialogFoldersDoc {
		respondError(w, http.StatusRequestEntityTooLarge, "doc too large")
		return
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(req.Doc, &probe); err != nil {
		respondError(w, http.StatusBadRequest, "doc must be a JSON object")
		return
	}
	if req.BaseVersion < 0 {
		respondError(w, http.StatusBadRequest, "invalid base_version")
		return
	}

	version, ok, err := s.database.SaveDialogFolders(user.ID, string(req.Doc), req.BaseVersion)
	if err != nil {
		log.Printf("dialog folders: save user=%d: %v", user.ID, err)
		respondError(w, http.StatusInternalServerError, "failed to save dialog folders")
		return
	}
	if !ok {
		doc, current, err := s.database.GetDialogFolders(user.ID)
		if err != nil {
			log.Printf("dialog folders: reload after conflict user=%d: %v", user.ID, err)
			respondError(w, http.StatusInternalServerError, "failed to load dialog folders")
			return
		}
		resp := dialogFoldersResponse{Version: current, Doc: json.RawMessage("null")}
		if doc != "" {
			resp.Doc = json.RawMessage(doc)
		}
		respondJSON(w, http.StatusConflict, resp)
		return
	}

	if s.notifyHub != nil {
		s.notifyHub.Publish(notify.Event{
			UserID: user.ID,
			Type:   notify.EventDialogFoldersChanged,
		})
	}
	respondJSON(w, http.StatusOK, dialogFoldersResponse{Version: version, Doc: req.Doc})
}
