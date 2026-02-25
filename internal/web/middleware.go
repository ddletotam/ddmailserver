package web

import (
	"context"
	"log"
	"net/http"
	"strings"
)

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
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

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

// Helper to get user ID from context
func getUserID(r *http.Request) int64 {
	userID, ok := r.Context().Value(userIDKey).(int64)
	if !ok {
		return 0
	}
	return userID
}
