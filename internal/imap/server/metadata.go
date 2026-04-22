package server

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/emersion/go-imap"
	imapserver "github.com/emersion/go-imap/server"
)

// Identity represents a user's email identity
type Identity struct {
	Email     string `json:"email"`
	Name      string `json:"name"`
	Signature string `json:"signature"`
	IsDefault bool   `json:"is_default"`
}

// MetadataExtension implements IMAP METADATA (RFC 5464) subset for DDMailServer.
type MetadataExtension struct{}

func NewMetadataExtension() *MetadataExtension {
	return &MetadataExtension{}
}

func (ext *MetadataExtension) Capabilities(c imapserver.Conn) []string {
	return []string{"METADATA"}
}

func (ext *MetadataExtension) Command(name string) imapserver.HandlerFactory {
	if strings.EqualFold(name, "GETMETADATA") {
		return func() imapserver.Handler {
			return &getMetadataHandler{}
		}
	}
	return nil
}

type getMetadataHandler struct {
	mailbox string
	entries []string
}

func (h *getMetadataHandler) Parse(fields []interface{}) error {
	if len(fields) < 2 {
		return fmt.Errorf("GETMETADATA requires at least 2 arguments")
	}

	h.mailbox, _ = imap.ParseString(fields[0])

	switch v := fields[1].(type) {
	case string:
		h.entries = []string{v}
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				h.entries = append(h.entries, s)
			}
		}
	default:
		return fmt.Errorf("invalid GETMETADATA entry format")
	}

	return nil
}

func (h *getMetadataHandler) Handle(conn imapserver.Conn) error {
	ctx := conn.Context()
	if ctx.User == nil {
		return imapserver.ErrNotAuthenticated
	}

	user, ok := ctx.User.(*User)
	if !ok {
		return fmt.Errorf("unexpected user type")
	}

	log.Printf("GETMETADATA: mailbox=%q entries=%v user=%s", h.mailbox, h.entries, user.username)

	for _, entry := range h.entries {
		value, err := getMetadataValue(user, entry)
		if err != nil {
			log.Printf("GETMETADATA: error for %s: %v", entry, err)
			continue
		}
		if value == "" {
			continue
		}

		// Send: * METADATA "" (/shared/vendor/ddmail/identities "json_value")
		resp := imap.NewUntaggedResp([]interface{}{
			imap.RawString("METADATA"),
			h.mailbox,
			[]interface{}{entry, value},
		})
		if err := conn.WriteResp(resp); err != nil {
			return fmt.Errorf("failed to write METADATA response: %w", err)
		}
	}

	return nil
}

func getMetadataValue(user *User, path string) (string, error) {
	switch path {
	case "/shared/vendor/ddmail/identities":
		return getIdentitiesJSON(user)
	case "/shared/vendor/ddmail/addressbooks":
		return "[]", nil // reserved
	case "/shared/vendor/ddmail/calendars":
		return "[]", nil // reserved
	default:
		return "", nil
	}
}

func getIdentitiesJSON(user *User) (string, error) {
	var identities []Identity

	// 1. Local mailboxes (local_part@domain)
	localMailboxes, err := user.database.GetMailboxesWithDomainByUserID(user.userID)
	if err != nil {
		log.Printf("METADATA identities: failed to get local mailboxes: %v", err)
	} else {
		for i, mb := range localMailboxes {
			if !mb.Enabled {
				continue
			}
			email := fmt.Sprintf("%s@%s", mb.LocalPart, mb.DomainName)
			identities = append(identities, Identity{
				Email:     email,
				Name:      getUserDisplayName(user),
				Signature: "",
				IsDefault: i == 0,
			})
		}
	}

	// 2. External accounts
	accounts, err := user.database.GetAccountsByUserID(user.userID)
	if err != nil {
		log.Printf("METADATA identities: failed to get accounts: %v", err)
	} else {
		for _, acc := range accounts {
			if acc.Email == "" || !acc.Enabled {
				continue
			}
			identities = append(identities, Identity{
				Email:     acc.Email,
				Name:      acc.Name,
				Signature: "",
				IsDefault: len(identities) == 0,
			})
		}
	}

	if len(identities) == 0 {
		identities = append(identities, Identity{
			Email:     user.username,
			Name:      user.username,
			Signature: "",
			IsDefault: true,
		})
	}

	data, err := json.Marshal(identities)
	if err != nil {
		return "", fmt.Errorf("marshal identities: %w", err)
	}

	return string(data), nil
}

func getUserDisplayName(user *User) string {
	u, err := user.database.GetUserByID(user.userID)
	if err != nil {
		return user.username
	}
	return u.Username
}

// Ensure interfaces
var _ imapserver.Extension = &MetadataExtension{}
var _ imapserver.Handler = &getMetadataHandler{}
