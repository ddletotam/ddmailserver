package client

import (
	"crypto/tls"
	"fmt"
	"log"
	"net/smtp"

	"github.com/yourusername/mailserver/internal/models"
)

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

	// Create authentication
	auth := smtp.PlainAuth(
		"",
		c.account.SMTPUsername,
		c.account.SMTPPassword,
		c.account.SMTPHost,
	)

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
