package web

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yourusername/mailserver/internal/models"
)

// RateLimiter provides IP-based rate limiting
type RateLimiter struct {
	requests map[string][]time.Time
	mu       sync.RWMutex
	limit    int           // max requests
	window   time.Duration // time window
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	rl := &RateLimiter{
		requests: make(map[string][]time.Time),
		limit:    limit,
		window:   window,
	}
	// Cleanup old entries periodically
	go rl.cleanup()
	return rl
}

// Allow checks if a request from the given IP is allowed
func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	windowStart := now.Add(-rl.window)

	// Filter out old requests
	var recent []time.Time
	for _, t := range rl.requests[ip] {
		if t.After(windowStart) {
			recent = append(recent, t)
		}
	}

	if len(recent) >= rl.limit {
		rl.requests[ip] = recent
		return false
	}

	rl.requests[ip] = append(recent, now)
	return true
}

// cleanup removes old entries periodically
func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(rl.window)
	for range ticker.C {
		rl.mu.Lock()
		now := time.Now()
		windowStart := now.Add(-rl.window)
		for ip, times := range rl.requests {
			var recent []time.Time
			for _, t := range times {
				if t.After(windowStart) {
					recent = append(recent, t)
				}
			}
			if len(recent) == 0 {
				delete(rl.requests, ip)
			} else {
				rl.requests[ip] = recent
			}
		}
		rl.mu.Unlock()
	}
}

type contextKey string

const userIDKey contextKey = "userID"
const usernameKey contextKey = "username"

// AuthMiddleware validates JWT tokens
func (s *Server) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Get token from Authorization header
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			respondError(w, http.StatusUnauthorized, "missing authorization header")
			return
		}

		// Check Bearer prefix
		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || parts[0] != "Bearer" {
			respondError(w, http.StatusUnauthorized, "invalid authorization header")
			return
		}

		tokenString := parts[1]

		// Validate token
		claims, err := ValidateToken(tokenString, s.jwtSecret)
		if err != nil {
			log.Printf("Token validation failed: %v", err)
			respondError(w, http.StatusUnauthorized, "invalid token")
			return
		}

		// Add user info to context
		ctx := context.WithValue(r.Context(), userIDKey, claims.UserID)
		ctx = context.WithValue(ctx, usernameKey, claims.Username)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// CORS middleware
func (s *Server) CORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		if origin != "" {
			isAllowed := false

			// Get the actual host (check X-Forwarded-Host for reverse proxy setups)
			host := r.Header.Get("X-Forwarded-Host")
			if host == "" {
				host = r.Host
			}

			// Check localhost for development
			if strings.HasPrefix(origin, "http://localhost") || strings.HasPrefix(origin, "http://127.0.0.1") {
				isAllowed = true
			}

			// Check if origin matches actual host (handles reverse proxy)
			hostWithoutPort := strings.Split(host, ":")[0]
			if strings.Contains(origin, hostWithoutPort) {
				isAllowed = true
			}

			if isAllowed {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			} else {
				log.Printf("CORS rejected origin: %s (host: %s, x-forwarded: %s)", origin, r.Host, r.Header.Get("X-Forwarded-Host"))
				http.Error(w, "CORS origin not allowed", http.StatusForbidden)
				return
			}
		}

		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, HX-Request, HX-Current-URL")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// LoggingMiddleware logs all requests
func (s *Server) LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}

// RateLimitMiddleware applies rate limiting to a handler
func (s *Server) RateLimitMiddleware(rl *RateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Extract IP from RemoteAddr or X-Forwarded-For
			ip := r.RemoteAddr
			if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
				ip = strings.Split(forwarded, ",")[0]
			}
			ip = strings.TrimSpace(strings.Split(ip, ":")[0])

			if !rl.Allow(ip) {
				log.Printf("Rate limit exceeded for IP: %s on %s", ip, r.URL.Path)
				http.Error(w, "Too many requests. Please try again later.", http.StatusTooManyRequests)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// Helper to get user ID from context
func getUserID(r *http.Request) int64 {
	// Try to get userID from JWT context (for API routes)
	userID, ok := r.Context().Value(userIDKey).(int64)
	if ok {
		return userID
	}

	// Try to get user from session context (for web routes)
	user, ok := r.Context().Value(userContextKey).(*models.User)
	if ok && user != nil {
		return user.ID
	}

	return 0
}
