package web

import "net/http"

// HandleDDMailDiscovery responds to /.well-known/ddmail for desktop client auto-detection.
// If the server responds with the expected JSON, the client switches to HTTP/2+WS mode.
func (s *Server) HandleDDMailDiscovery(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ddmail":   true,
		"version":  1,
		"api_base": "/api/desktop/v1",
		"ws_path":  "/api/desktop/v1/ws",
		"features": []string{"delta-sync", "server-search", "push-all-folders", "thread-grouping"},
	})
}
