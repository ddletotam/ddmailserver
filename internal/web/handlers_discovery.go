package web

import (
	"net/http"
	"strings"
)

// HandleDDMailDiscovery responds to /.well-known/ddmail for desktop client auto-detection.
// If the server responds with the expected JSON, the client switches to HTTP/2+WS mode.
//
// With ?address= it also says what that address is here: a hosted mailbox
// with its own account ("address_kind": "mailbox") signs in on IMAP/SMTP,
// not as a DDMail user — the client must add it as an IMAP/SMTP account
// (docs/mailbox-accounts-plan.md, step 4). Clients that do not send the
// parameter get the old answer.
func (s *Server) HandleDDMailDiscovery(w http.ResponseWriter, r *http.Request) {
	resp := map[string]interface{}{
		"ddmail":   true,
		"version":  1,
		"api_base": "/api/desktop/v1",
		"ws_path":  "/api/desktop/v1/ws",
		"features": []string{"delta-sync", "server-search", "push-all-folders", "thread-grouping", "push-token", "mailbox-accounts"},
	}
	if addr := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("address"))); strings.Contains(addr, "@") && s.database != nil {
		kind := "user"
		if u, err := s.database.GetUserByUsername(addr); err == nil && u.IsMailbox() {
			kind = "mailbox"
			p := s.publicEndpoints
			resp["imap"] = map[string]interface{}{"host": p.Hostname, "port": p.IMAPPort, "security": "ssl"}
			resp["smtp"] = map[string]interface{}{"host": p.Hostname, "port": p.SMTPPort, "security": "ssl"}
		}
		resp["address_kind"] = kind
	}
	respondJSON(w, http.StatusOK, resp)
}
