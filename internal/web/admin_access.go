package web

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
)

// Administrator accounts are separate from users (migration 061,
// docs/server-admin-split-plan.md): they see /admin and nothing else.

// adminAllowedAPI lists the API prefixes an administrator may call outside
// /api/admin: changing their own password and language, and the OAuth app
// settings the admin page saves.
var adminAllowedAPI = []string{
	"/api/admin",
	"/api/settings/password",
	"/api/settings/language",
	"/api/settings/oauth/",
}

func adminMayCallAPI(path string) bool {
	for _, p := range adminAllowedAPI {
		if path == p || strings.HasPrefix(path, p+"/") || (strings.HasSuffix(p, "/") && strings.HasPrefix(path, p)) {
			return true
		}
	}
	return false
}

// adminWebPath: the web pages an administrator may open.
func adminWebPath(path string) bool {
	return path == "/admin" || strings.HasPrefix(path, "/admin/") || path == "/logout"
}

func parseNetworks(entries []string) ([]*net.IPNet, error) {
	var nets []*net.IPNet
	var bad []string
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.Contains(e, "/") {
			if ip := net.ParseIP(e); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
				continue
			}
		}
		_, n, err := net.ParseCIDR(e)
		if err != nil {
			bad = append(bad, e)
			continue
		}
		nets = append(nets, n)
	}
	if len(bad) > 0 {
		return nets, fmt.Errorf("admin_networks: not an IP or CIDR: %s", strings.Join(bad, ", "))
	}
	return nets, nil
}

// adminNetworkAllowed reports whether the request may reach the admin pages.
func (s *Server) adminNetworkAllowed(r *http.Request) bool {
	if len(s.adminNetworks) == 0 {
		return true
	}
	ip := net.ParseIP(s.clientIP.FromRequest(r))
	if ip == nil {
		return false
	}
	for _, n := range s.adminNetworks {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// auditAdmin records an administrator action; a failed write is logged, the
// action itself already happened.
func (s *Server) auditAdmin(r *http.Request, admin *models.User, action, targetType string, targetID int64, details string) {
	if admin == nil {
		return
	}
	a := db.AdminAction{
		AdminID: admin.ID, AdminName: admin.Username, Action: action,
		TargetType: targetType, TargetID: targetID, Details: details,
		IP: s.clientIP.FromRequest(r),
	}
	if err := s.database.LogAdminAction(a); err != nil {
		log.Printf("admin audit: %v", err)
	}
	log.Printf("admin: %s %s %s#%d %s (by %s)", action, targetType, details, targetID, a.IP, admin.Username)
}
