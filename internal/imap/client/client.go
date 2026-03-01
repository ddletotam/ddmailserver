package client

import (
	"crypto/tls"
	"fmt"
	"log"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/yourusername/mailserver/internal/models"
)

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

	// Login
	log.Printf("Authenticating as %s", c.account.IMAPUsername)
	if err := c.conn.Login(c.account.IMAPUsername, c.account.IMAPPassword); err != nil {
		c.conn.Logout()
		return fmt.Errorf("failed to login: %w", err)
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
