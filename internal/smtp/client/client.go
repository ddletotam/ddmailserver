package client

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"log"
	"net/smtp"

	"github.com/yourusername/mailserver/internal/models"
)

// oauthBearerAuth implements smtp.Auth for OAUTHBEARER (RFC 7628)
type oauthBearerAuth struct {
	username string
	token    string
	host     string
	port     int
}

// Start begins OAUTHBEARER authentication
func (a *oauthBearerAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	// OAUTHBEARER initial response format (RFC 7628):
	// n,a=<authzid>,^Ahost=<host>^Aport=<port>^Aauth=Bearer <token>^A^A
	// where ^A is ASCII 0x01
	response := fmt.Sprintf("n,a=%s,\x01host=%s\x01port=%d\x01auth=Bearer %s\x01\x01",
		a.username, a.host, a.port, a.token)
	return "OAUTHBEARER", []byte(response), nil
}

// Next handles server challenges (not expected for OAUTHBEARER)
func (a *oauthBearerAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		// Server sent an error response
		return nil, fmt.Errorf("OAUTHBEARER error: %s", string(fromServer))
	}
	return nil, nil
}

// xoauth2Auth implements smtp.Auth for XOAUTH2 (Gmail-specific)
type xoauth2Auth struct {
	username string
	token    string
}

// Start begins XOAUTH2 authentication
func (a *xoauth2Auth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	// XOAUTH2 format: base64("user=" + email + "\x01auth=Bearer " + token + "\x01\x01")
	authString := fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", a.username, a.token)
	return "XOAUTH2", []byte(authString), nil
}

// Next handles server challenges
func (a *xoauth2Auth) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		// Server sent an error response (base64 encoded JSON)
		decoded, err := base64.StdEncoding.DecodeString(string(fromServer))
		if err != nil {
			return nil, fmt.Errorf("XOAUTH2 error: %s", string(fromServer))
		}
		return nil, fmt.Errorf("XOAUTH2 error: %s", string(decoded))
	}
	return nil, nil
}

// Client wraps the SMTP client for external mail servers
type Client struct {
	account *models.Account
}

// New creates a new SMTP client for an account
func New(account *models.Account) *Client {
	return &Client{
		account: account,
	}
}

// Send sends an email through the external SMTP server
func (c *Client) Send(from string, to []string, message []byte) error {
	addr := fmt.Sprintf("%s:%d", c.account.SMTPHost, c.account.SMTPPort)

	log.Printf("Sending email via SMTP %s", addr)

	// Create authentication based on auth type
	var auth smtp.Auth
	if c.account.IsOAuth() {
		// Use XOAUTH2 for Gmail (more widely supported than OAUTHBEARER)
		log.Printf("Using XOAUTH2 authentication for %s", c.account.SMTPUsername)
		auth = &xoauth2Auth{
			username: c.account.SMTPUsername,
			token:    c.account.OAuthAccessToken,
		}
	} else {
		// Use PLAIN auth for password-based authentication
		auth = smtp.PlainAuth(
			"",
			c.account.SMTPUsername,
			c.account.SMTPPassword,
			c.account.SMTPHost,
		)
	}

	var err error
	if c.account.SMTPTLS {
		// Use TLS (port 465 or 587 with STARTTLS)
		err = c.sendTLS(addr, auth, from, to, message)
	} else {
		// Plain SMTP (usually port 25, not recommended)
		err = smtp.SendMail(addr, auth, from, to, message)
	}

	if err != nil {
		return fmt.Errorf("failed to send email: %w", err)
	}

	log.Printf("Email sent successfully from %s to %v", from, to)
	return nil
}

// sendTLS sends email with TLS encryption
func (c *Client) sendTLS(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	// Connect to the server
	client, err := smtp.Dial(addr)
	if err != nil {
		return fmt.Errorf("failed to dial: %w", err)
	}
	defer client.Close()

	// Start TLS
	tlsConfig := &tls.Config{
		ServerName: c.account.SMTPHost,
	}

	if err = client.StartTLS(tlsConfig); err != nil {
		return fmt.Errorf("failed to start TLS: %w", err)
	}

	// Authenticate
	if auth != nil {
		if err = client.Auth(auth); err != nil {
			return fmt.Errorf("failed to authenticate: %w", err)
		}
	}

	// Set sender
	if err = client.Mail(from); err != nil {
		return fmt.Errorf("failed to set sender: %w", err)
	}

	// Set recipients
	for _, recipient := range to {
		if err = client.Rcpt(recipient); err != nil {
			return fmt.Errorf("failed to set recipient %s: %w", recipient, err)
		}
	}

	// Send message body
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("failed to get data writer: %w", err)
	}

	_, err = w.Write(msg)
	if err != nil {
		return fmt.Errorf("failed to write message: %w", err)
	}

	err = w.Close()
	if err != nil {
		return fmt.Errorf("failed to close data writer: %w", err)
	}

	// Quit
	return client.Quit()
}
