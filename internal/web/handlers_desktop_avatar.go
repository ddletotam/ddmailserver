package web

import (
	"net/http"
	"strconv"

	"github.com/yourusername/mailserver/internal/avatar"
)

// HandleDesktopAvatar resolves an avatar for the given email by walking the
// source chain (CardDAV → Libravatar → Gravatar → BIMI → favicon) and
// streams the image bytes back. Returns 204 No Content when nothing was
// found so the client can fall back to its initial-bubble render without
// treating it as an error.
//
// URL: GET /api/desktop/v1/avatars?email=info@apple.com
// Query param keeps the address safe across nginx and the router; encoding
// `@` in a path segment depends on `UseEncodedPath` plumbing we don't control.
func (s *Server) HandleDesktopAvatar(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	email := r.URL.Query().Get("email")
	if email == "" {
		respondError(w, http.StatusBadRequest, "email required")
		return
	}

	fetcher := avatar.New(s.database)
	result, err := fetcher.Get(r.Context(), user.ID, email)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if result.IsEmpty() {
		// 204 keeps response bodies small (and parseable as "no avatar")
		// without making it look like an HTTP error.
		w.Header().Set("X-Avatar-Source", "none")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.Header().Set("Content-Type", result.MIME)
	w.Header().Set("Content-Length", strconv.Itoa(len(result.Data)))
	w.Header().Set("X-Avatar-Source", result.Source)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result.Data)
}
