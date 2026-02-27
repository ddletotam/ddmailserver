package web

import (
	"context"
	"net/http"

	"github.com/yourusername/mailserver/internal/models"
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

// GetUserFromContext retrieves user from context
func (s *Server) GetUserFromContext(ctx context.Context) *models.User {
	user, ok := ctx.Value(userContextKey).(*models.User)
	if !ok {
		return nil
	}
	return user
}

// SetSessionCookie sets the session cookie with JWT
func (s *Server) SetSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		MaxAge:   86400 * 7, // 7 days
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// Secure:   true, // Enable in production with HTTPS
	})
}
