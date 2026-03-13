package client

import (
	"crypto/tls"
	"fmt"
	"log"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-sasl"
	"github.com/yourusername/mailserver/internal/models"
)

// xoauth2Client implements XOAUTH2 SASL authentication for Gmail
type xoauth2Client struct {
	username string
	token    string
}

// Start begins XOAUTH2 authentication
func (c *xoauth2Client) Start() (mech string, ir []byte, err error) {
	// XOAUTH2 format: "user=" + email + "\x01auth=Bearer " + token + "\x01\x01"
	authString := fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", c.username, c.token)
	return "XOAUTH2", []byte(authString), nil
}

// Next handles server challenges (not expected for XOAUTH2)
func (c *xoauth2Client) Next(challenge []byte) (response []byte, err error) {
	// If server sends a challenge, it's an error message
	return nil, fmt.Errorf("XOAUTH2 error from server: %s", string(challenge))
}

// newXOAuth2Client creates a new XOAUTH2 SASL client
func newXOAuth2Client(username, token string) sasl.Client {
	return &xoauth2Client{
		username: username,
		token:    token,
	}
}

// Client wraps the IMAP client for external mail servers
type Client struct {
	account *models.Account
	conn    *client.Client
}

// New creates a new IMAP client for an account
func New(account *models.Account) (*Client, error) {
	return &Client{
		account: account,
	}, nil
}

// Connect establishes connection to the IMAP server
func (c *Client) Connect() error {
	var conn *client.Client
	var err error

	addr := fmt.Sprintf("%s:%d", c.account.IMAPHost, c.account.IMAPPort)

	if c.account.IMAPTLS {
		// Connect with TLS
		log.Printf("Connecting to IMAP server %s with TLS", addr)
		conn, err = client.DialTLS(addr, &tls.Config{
			ServerName: c.account.IMAPHost,
		})
	} else {
		// Connect without TLS
		log.Printf("Connecting to IMAP server %s without TLS", addr)
		conn, err = client.Dial(addr)
	}

	if err != nil {
		return fmt.Errorf("failed to connect to IMAP server: %w", err)
	}

	c.conn = conn

	// Authenticate based on auth type
	if c.account.IsOAuth() {
		// Try XOAUTH2 first (Gmail uses this)
		log.Printf("Authenticating with XOAUTH2 as %s", c.account.IMAPUsername)
		xoauth2 := newXOAuth2Client(c.account.IMAPUsername, c.account.OAuthAccessToken)
		err := c.conn.Authenticate(xoauth2)
		if err != nil {
			// Fall back to OAUTHBEARER (RFC 7628)
			log.Printf("XOAUTH2 failed, trying OAUTHBEARER: %v", err)
			oauthbearer := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
				Username: c.account.IMAPUsername,
				Token:    c.account.OAuthAccessToken,
				Host:     c.account.IMAPHost,
				Port:     c.account.IMAPPort,
			})
			if err := c.conn.Authenticate(oauthbearer); err != nil {
				c.conn.Logout()
				return fmt.Errorf("failed to authenticate with OAuth: %w", err)
			}
		}
	} else {
		// Use plain LOGIN for password-based auth
		log.Printf("Authenticating as %s", c.account.IMAPUsername)
		if err := c.conn.Login(c.account.IMAPUsername, c.account.IMAPPassword); err != nil {
			c.conn.Logout()
			return fmt.Errorf("failed to login: %w", err)
		}
	}

	log.Printf("Successfully connected to %s", c.account.Email)
	return nil
}

// Disconnect closes the connection
func (c *Client) Disconnect() error {
	if c.conn != nil {
		log.Printf("Disconnecting from %s", c.account.Email)
		return c.conn.Logout()
	}
	return nil
}

// ListFolders returns all mailboxes
func (c *Client) ListFolders() ([]*imap.MailboxInfo, error) {
	mailboxes := make(chan *imap.MailboxInfo, 100)
	done := make(chan error, 1)

	go func() {
		done <- c.conn.List("", "*", mailboxes)
	}()

	var folders []*imap.MailboxInfo
	for m := range mailboxes {
		folders = append(folders, m)
	}

	if err := <-done; err != nil {
		return nil, fmt.Errorf("failed to list folders: %w", err)
	}

	return folders, nil
}

// SelectFolder selects a mailbox
func (c *Client) SelectFolder(name string) (*imap.MailboxStatus, error) {
	mbox, err := c.conn.Select(name, false)
	if err != nil {
		return nil, fmt.Errorf("failed to select folder %s: %w", name, err)
	}
	return mbox, nil
}

// FetchMessages fetches messages from the current mailbox by sequence numbers
// Returns a channel of messages and an error channel for async error handling
func (c *Client) FetchMessages(seqSet *imap.SeqSet, items []imap.FetchItem) (chan *imap.Message, chan error) {
	messages := make(chan *imap.Message, 100)
	done := make(chan error, 1)

	go func() {
		done <- c.conn.Fetch(seqSet, items, messages)
	}()

	return messages, done
}

// FetchMessagesByUID fetches messages from the current mailbox by UIDs
// Returns a channel of messages and an error channel for async error handling
func (c *Client) FetchMessagesByUID(uidSet *imap.SeqSet, items []imap.FetchItem) (chan *imap.Message, chan error) {
	messages := make(chan *imap.Message, 100)
	done := make(chan error, 1)

	go func() {
		done <- c.conn.UidFetch(uidSet, items, messages)
	}()

	return messages, done
}

// GetConnection returns the underlying IMAP connection
func (c *Client) GetConnection() *client.Client {
	return c.conn
}

// IsConnected checks if the client is connected
func (c *Client) IsConnected() bool {
	return c.conn != nil && c.conn.State() == imap.AuthenticatedState
}

// StoreFlags updates flags on a message on the remote IMAP server
// This is used for bidirectional sync - pushing local flag changes to source
func (c *Client) StoreFlags(folder string, uid uint32, seen, flagged, answered, deleted bool) error {
	// Select folder
	if _, err := c.conn.Select(folder, false); err != nil {
		return fmt.Errorf("failed to select folder %s: %w", folder, err)
	}

	// Build flags list
	var flags []interface{}
	if seen {
		flags = append(flags, imap.SeenFlag)
	}
	if flagged {
		flags = append(flags, imap.FlaggedFlag)
	}
	if answered {
		flags = append(flags, imap.AnsweredFlag)
	}
	if deleted {
		flags = append(flags, imap.DeletedFlag)
	}

	// Create UID set for single message
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)

	// Use SetFlags to replace all flags (not add/remove)
	item := imap.FormatFlagsOp(imap.SetFlags, true)
	if err := c.conn.UidStore(seqSet, item, flags, nil); err != nil {
		return fmt.Errorf("failed to store flags for UID %d: %w", uid, err)
	}

	return nil
}
