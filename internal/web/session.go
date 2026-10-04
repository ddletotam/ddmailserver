package web

import (
	"context"
	"net/http"

	"github.com/ddletotam/ddmailserver/internal/config"

	"github.com/ddletotam/ddmailserver/internal/models"
)

const userContextKey contextKey = "user"

// SessionMiddleware extracts user from JWT cookie
func (s *Server) SessionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Try to get JWT from cookie
		cookie, err := r.Cookie("session")
		if err == nil && cookie.Value != "" {
			// Validate JWT and get user
			claims, err := ValidateToken(cookie.Value, s.jwtSecret)
			if err == nil {
				user, err := s.database.GetUserByID(claims.UserID)
				if err == nil {
					// Add user to context
					ctx := context.WithValue(r.Context(), userContextKey, user)
					r = r.WithContext(ctx)
				}
			}
		}

		next.ServeHTTP(w, r)
	})
}

// WebAuthMiddleware protects web routes (redirects to login if not authenticated)
func (s *Server) WebAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := s.GetUserFromContext(r.Context())
		if user == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// APIAuthMiddleware protects API routes (returns JSON error if not authenticated)
func (s *Server) APIAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := s.GetUserFromContext(r.Context())
		if user == nil {
			respondError(w, http.StatusUnauthorized, "unauthorized - please login")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// WebAdminMiddleware gates web routes that require admin (non-admins get 404
// rather than 403 — we don't even acknowledge the page exists).
func (s *Server) WebAdminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := s.GetUserFromContext(r.Context())
		if user == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if !user.IsAdmin() {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// APIAdminMiddleware gates JSON API routes that require admin.
func (s *Server) APIAdminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := s.GetUserFromContext(r.Context())
		if user == nil {
			respondError(w, http.StatusUnauthorized, "unauthorized - please login")
			return
		}
		if !user.IsAdmin() {
			respondError(w, http.StatusForbidden, "admin only")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// GetUserFromContext retrieves user from context
func (s *Server) GetUserFromContext(ctx context.Context) *models.User {
	user, ok := ctx.Value(userContextKey).(*models.User)
	if !ok {
		return nil
	}
	return user
}

// SetSessionCookie sets the session cookie with JWT
func (s *Server) SetSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		MaxAge:   86400 * 7, // 7 days
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode, // Lax allows OAuth redirects while protecting against CSRF
		Secure:   s.secureCookie(r),
	})
}

// clearSessionCookie expires the session cookie.
func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.secureCookie(r),
	})
}

// secureCookie decides the Secure flag for every cookie this server sets
// (session, OAuth state and redirect URI), from one rule:
//
//   - server.public.secure_cookies "true"/"false" forces it;
//   - "auto" (default): Secure when the client is known to use HTTPS (TLS
//     here, or X-Forwarded-Proto: https from a trusted proxy). When the
//     scheme is unknown — e.g. a proxy that sends no X-Forwarded-Proto —
//     it stays Secure unless the request addresses a loopback host, so
//     production never silently loses the flag; only an explicit
//     "http" from a trusted proxy or local development turns it off.
//
// The flag used to come from r.TLS alone (never set behind nginx) for the
// OAuth cookies and from the listen address for the session cookie.
func (s *Server) secureCookie(r *http.Request) bool {
	switch s.publicEndpoints.SecureCookies {
	case config.SecureCookiesTrue:
		return true
	case config.SecureCookiesFalse:
		return false
	}
	switch s.clientIP.RequestScheme(r) {
	case "https":
		return true
	case "http":
		return false
	}
	return !isLoopbackName(hostWithoutPort(r.Host))
}
