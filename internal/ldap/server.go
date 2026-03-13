package ldap

import (
	"fmt"
	"log"
	"net"
	"strings"

	"github.com/nmcclain/ldap"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"golang.org/x/crypto/bcrypt"
)

// Server is an LDAP server for address book lookups
type Server struct {
	database *db.DB
	baseDN   string
	port     int
	listener net.Listener
	server   *ldap.Server
}

// Config holds LDAP server configuration
type Config struct {
	Port   int
	BaseDN string
}

// New creates a new LDAP server
func New(database *db.DB, config Config) *Server {
	if config.Port == 0 {
		config.Port = 10389
	}
	if config.BaseDN == "" {
		config.BaseDN = "dc=mail,dc=letotam,dc=ru"
	}

	return &Server{
		database: database,
		baseDN:   config.BaseDN,
		port:     config.Port,
	}
}

// Start starts the LDAP server
func (s *Server) Start() error {
	server := ldap.NewServer()

	// Register handlers
	server.BindFunc("", s)
	server.SearchFunc("", s)
	server.CloseFunc("", s)

	s.server = server

	addr := fmt.Sprintf(":%d", s.port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	s.listener = listener

	log.Printf("LDAP server listening on %s (BaseDN: %s)", addr, s.baseDN)

	go func() {
		if err := server.Serve(listener); err != nil {
			log.Printf("LDAP server error: %v", err)
		}
	}()

	return nil
}

// Stop stops the LDAP server
func (s *Server) Stop() error {
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

// Bind handles LDAP bind requests (authentication)
func (s *Server) Bind(bindDN, bindSimplePw string, conn net.Conn) (ldap.LDAPResultCode, error) {
	log.Printf("LDAP Bind: DN=%s", bindDN)

	// Allow anonymous bind for searches
	if bindDN == "" {
		return ldap.LDAPResultSuccess, nil
	}

	// Parse bind DN to extract username
	// Expected format: uid=username,ou=users,dc=mail,dc=letotam,dc=ru
	// or just: username@domain
	username := extractUsername(bindDN)
	if username == "" {
		return ldap.LDAPResultInvalidCredentials, nil
	}

	// Authenticate user
	user, err := s.database.GetUserByUsername(username)
	if err != nil {
		log.Printf("LDAP Bind failed: user not found: %s", username)
		return ldap.LDAPResultInvalidCredentials, nil
	}

	// Verify password using bcrypt
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(bindSimplePw)); err != nil {
		log.Printf("LDAP Bind failed: invalid password for %s", username)
		return ldap.LDAPResultInvalidCredentials, nil
	}

	log.Printf("LDAP Bind successful: %s", username)
	return ldap.LDAPResultSuccess, nil
}

// Search handles LDAP search requests
func (s *Server) Search(boundDN string, req ldap.SearchRequest, conn net.Conn) (ldap.ServerSearchResult, error) {
	log.Printf("LDAP Search: BaseDN=%s, Filter=%s, Scope=%d", req.BaseDN, req.Filter, req.Scope)

	result := ldap.ServerSearchResult{
		Entries:    []*ldap.Entry{},
		Referrals:  []string{},
		Controls:   []ldap.Control{},
		ResultCode: ldap.LDAPResultSuccess,
	}

	// Extract user from bound DN
	username := extractUsername(boundDN)
	if username == "" {
		// Anonymous search - return empty results
		return result, nil
	}

	// Get user
	user, err := s.database.GetUserByUsername(username)
	if err != nil {
		log.Printf("LDAP Search: user not found: %s", username)
		return result, nil
	}

	// Parse filter to extract search term
	searchTerm := extractSearchTerm(req.Filter)
	if searchTerm == "" {
		// Return all contacts for the user (limited)
		searchTerm = "*"
	}

	// Search contacts
	contacts, err := s.database.SearchContacts(user.ID, strings.ReplaceAll(searchTerm, "*", ""), 100)
	if err != nil {
		log.Printf("LDAP Search error: %v", err)
		return result, nil
	}

	log.Printf("LDAP Search: found %d contacts for term '%s'", len(contacts), searchTerm)

	// Convert contacts to LDAP entries
	for _, contact := range contacts {
		entry := s.contactToEntry(contact)
		if entry != nil {
			result.Entries = append(result.Entries, entry)
		}
	}

	return result, nil
}

// Close handles LDAP connection close
func (s *Server) Close(boundDN string, conn net.Conn) error {
	log.Printf("LDAP connection closed: %s", boundDN)
	return nil
}

// contactToEntry converts a contact to an LDAP entry
func (s *Server) contactToEntry(contact *models.Contact) *ldap.Entry {
	if contact.Email == "" && contact.FullName == "" {
		return nil
	}

	// Build DN
	cn := contact.FullName
	if cn == "" {
		cn = contact.Email
	}
	dn := fmt.Sprintf("cn=%s,ou=contacts,%s", escapeDN(cn), s.baseDN)

	attrs := []*ldap.EntryAttribute{
		{Name: "objectClass", Values: []string{"inetOrgPerson", "organizationalPerson", "person", "top"}},
		{Name: "cn", Values: []string{cn}},
	}

	// Add email
	if contact.Email != "" {
		attrs = append(attrs, &ldap.EntryAttribute{Name: "mail", Values: []string{contact.Email}})
	}

	// Add display name
	if contact.FullName != "" {
		attrs = append(attrs, &ldap.EntryAttribute{Name: "displayName", Values: []string{contact.FullName}})
	}

	// Add given name
	if contact.GivenName != "" {
		attrs = append(attrs, &ldap.EntryAttribute{Name: "givenName", Values: []string{contact.GivenName}})
	}

	// Add family name (sn is required for inetOrgPerson)
	sn := contact.FamilyName
	if sn == "" {
		sn = contact.FullName
		if sn == "" {
			sn = contact.Email
		}
	}
	attrs = append(attrs, &ldap.EntryAttribute{Name: "sn", Values: []string{sn}})

	// Add phone
	if contact.Phone != "" {
		attrs = append(attrs, &ldap.EntryAttribute{Name: "telephoneNumber", Values: []string{contact.Phone}})
	}

	// Add organization
	if contact.Organization != "" {
		attrs = append(attrs, &ldap.EntryAttribute{Name: "o", Values: []string{contact.Organization}})
	}

	// Add title
	if contact.Title != "" {
		attrs = append(attrs, &ldap.EntryAttribute{Name: "title", Values: []string{contact.Title}})
	}

	return &ldap.Entry{
		DN:         dn,
		Attributes: attrs,
	}
}

// extractUsername extracts username from a bind DN
func extractUsername(bindDN string) string {
	if bindDN == "" {
		return ""
	}

	// Check if it's an email address directly
	if strings.Contains(bindDN, "@") && !strings.Contains(bindDN, "=") {
		return bindDN
	}

	// Parse DN format: uid=user@domain,ou=users,dc=...
	// or: cn=user@domain,ou=users,dc=...
	bindDN = strings.ToLower(bindDN)
	parts := strings.Split(bindDN, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "uid=") {
			return strings.TrimPrefix(part, "uid=")
		}
		if strings.HasPrefix(part, "cn=") {
			return strings.TrimPrefix(part, "cn=")
		}
		if strings.HasPrefix(part, "mail=") {
			return strings.TrimPrefix(part, "mail=")
		}
	}

	// Just return the first part if nothing matches
	if len(parts) > 0 && strings.Contains(parts[0], "=") {
		return strings.Split(parts[0], "=")[1]
	}

	return bindDN
}

// extractSearchTerm extracts the search term from an LDAP filter
func extractSearchTerm(filter string) string {
	// Common filter formats:
	// (cn=*john*)
	// (mail=*john*)
	// (&(objectClass=person)(cn=*john*))
	// (|(cn=*john*)(mail=*john*)(sn=*john*))

	filter = strings.ToLower(filter)

	// Look for equality or substring filters
	for _, attr := range []string{"cn", "mail", "givenname", "sn", "displayname"} {
		prefix := "(" + attr + "="
		idx := strings.Index(filter, prefix)
		if idx >= 0 {
			start := idx + len(prefix)
			end := strings.Index(filter[start:], ")")
			if end > 0 {
				value := filter[start : start+end]
				// Remove wildcards for the search
				return strings.Trim(value, "*")
			}
		}
	}

	return ""
}

// escapeDN escapes special characters in DN components
func escapeDN(s string) string {
	// Characters that need escaping in DN: , + " \ < > ;
	replacer := strings.NewReplacer(
		",", "\\,",
		"+", "\\+",
		"\"", "\\\"",
		"\\", "\\\\",
		"<", "\\<",
		">", "\\>",
		";", "\\;",
	)
	return replacer.Replace(s)
}

// DomainToBaseDN converts a domain name to LDAP base DN
// e.g., mail.example.com -> dc=mail,dc=example,dc=com
func DomainToBaseDN(domain string) string {
	parts := strings.Split(domain, ".")
	var dnParts []string
	for _, part := range parts {
		dnParts = append(dnParts, "dc="+part)
	}
	return strings.Join(dnParts, ",")
}
