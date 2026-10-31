package web

import (
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/ddletotam/ddmailserver/internal/notify"
)

// maxIdentityGroupsDoc caps the stored document. Signatures are HTML with
// inline images (base64), so a few hundred KB is plausible; 4 MB is far
// beyond any honest use.
const maxIdentityGroupsDoc = 4 << 20

// identityGroupsResponse is both the GET answer and the PUT answer.
// Doc is the client's own JSON, passed through untouched (null when the user
// never saved groups).
type identityGroupsResponse struct {
	Version int64           `json:"version"`
	Doc     json.RawMessage `json:"doc"`
}

type identityGroupsPut struct {
	BaseVersion int64           `json:"base_version"`
	Doc         json.RawMessage `json:"doc"`
}

// HandleDesktopIdentityGroups returns the identity-groups document (names,
// colours, signatures, address order of the desktop sending identities). The
// server does not interpret it: the client collects addresses from accounts
// the server may never see.
func (s *Server) HandleDesktopIdentityGroups(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	doc, version, err := s.database.GetIdentityGroups(user.ID)
	if err != nil {
		log.Printf("identity groups: get user=%d: %v", user.ID, err)
		respondError(w, http.StatusInternalServerError, "failed to load identity groups")
		return
	}
	resp := identityGroupsResponse{Version: version, Doc: json.RawMessage("null")}
	if doc != "" {
		resp.Doc = json.RawMessage(doc)
	}
	respondJSON(w, http.StatusOK, resp)
}

// HandleDesktopIdentityGroupsSave replaces the document if base_version is
// still current. A stale base answers 409 with the current version and
// document, so the client can reapply its change on top instead of
// silently overwriting another station's edit.
func (s *Server) HandleDesktopIdentityGroupsSave(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxIdentityGroupsDoc+4096))
	if err != nil {
		respondError(w, http.StatusBadRequest, "failed to read body")
		return
	}
	var req identityGroupsPut
	if err := json.Unmarshal(body, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if len(req.Doc) == 0 || string(req.Doc) == "null" {
		respondError(w, http.StatusBadRequest, "doc is required")
		return
	}
	if len(req.Doc) > maxIdentityGroupsDoc {
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

	version, ok, err := s.database.SaveIdentityGroups(user.ID, string(req.Doc), req.BaseVersion)
	if err != nil {
		log.Printf("identity groups: save user=%d: %v", user.ID, err)
		respondError(w, http.StatusInternalServerError, "failed to save identity groups")
		return
	}
	if !ok {
		doc, current, err := s.database.GetIdentityGroups(user.ID)
		if err != nil {
			log.Printf("identity groups: reload after conflict user=%d: %v", user.ID, err)
			respondError(w, http.StatusInternalServerError, "failed to load identity groups")
			return
		}
		resp := identityGroupsResponse{Version: current, Doc: json.RawMessage("null")}
		if doc != "" {
			resp.Doc = json.RawMessage(doc)
		}
		respondJSON(w, http.StatusConflict, resp)
		return
	}

	if s.notifyHub != nil {
		s.notifyHub.Publish(notify.Event{
			UserID: user.ID,
			Type:   notify.EventIdentityGroupsChanged,
		})
	}
	respondJSON(w, http.StatusOK, identityGroupsResponse{Version: version, Doc: req.Doc})
}
