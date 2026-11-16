package web

import (
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strings"
)

// RandomPassword: n characters without look-alikes (0/O, 1/l/I).
func RandomPassword(n int) (string, error) {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, n)
	for i := range b {
		k, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", fmt.Errorf("random: %w", err)
		}
		b[i] = alphabet[k.Int64()]
	}
	return string(b), nil
}

// HandleAdminCreateAdmin makes another administrator account; its password
// is generated and returned once.
func (s *Server) HandleAdminCreateAdmin(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("username"))
	if name == "" || len(name) > MaxUsernameLength || strings.ContainsAny(name, " @") {
		respondError(w, http.StatusBadRequest, "login must be non-empty, without spaces or @")
		return
	}
	if _, err := s.database.GetUserByUsername(name); err == nil {
		respondError(w, http.StatusConflict, "login is taken")
		return
	}
	password, err := RandomPassword(20)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "password generation failed")
		return
	}
	hash, err := HashPassword(password)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "password hashing failed")
		return
	}
	u, err := s.database.CreateAdmin(name, hash)
	if err != nil {
		log.Printf("create admin: %v", err)
		respondError(w, http.StatusInternalServerError, "create failed")
		return
	}
	s.auditAdmin(r, s.GetUserFromContext(r.Context()), "admin.create", "admin", u.ID, name)
	respondJSON(w, http.StatusCreated, map[string]string{"username": name, "password": password})
}

// HandleAdminDeleteAdmin removes another administrator; never yourself and
// never the last one.
func (s *Server) HandleAdminDeleteAdmin(w http.ResponseWriter, r *http.Request) {
	id, ok := s.adminParseUserID(w, r, false)
	if !ok {
		return
	}
	target, err := s.database.GetUserByID(id)
	if err != nil || !target.IsAdmin() {
		respondError(w, http.StatusNotFound, "administrator not found")
		return
	}
	n, err := s.database.CountAdmins()
	if err != nil || n <= 1 {
		respondError(w, http.StatusBadRequest, "cannot delete the last administrator")
		return
	}
	if err := s.database.DeleteUser(id); err != nil {
		log.Printf("delete admin: %v", err)
		respondError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	s.revokeSessions(id)
	s.auditAdmin(r, s.GetUserFromContext(r.Context()), "admin.delete", "admin", id, target.Username)
	respondJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}
