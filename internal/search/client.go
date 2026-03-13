package search

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/yourusername/mailserver/internal/config"
)

// Client is a Meilisearch client
type Client struct {
	host   string
	apiKey string
	http   *http.Client
}

// New creates a new Meilisearch client
func New(cfg *config.MeilisearchConfig) *Client {
	return &Client{
		host:   cfg.Host,
		apiKey: cfg.APIKey,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// IndexDocument represents a document to be indexed
type IndexDocument struct {
	ID              int64   `json:"id"`
	UserID          int64   `json:"user_id"`
	FolderID        int64   `json:"folder_id"`
	AccountID       int64   `json:"account_id"`
	Subject         string  `json:"subject"`
	From            string  `json:"from"`
	To              string  `json:"to"`
	Cc              string  `json:"cc"`
	Body            string  `json:"body"`
	Date            int64   `json:"date"` // Unix timestamp for sorting
	DateISO         string  `json:"date_iso"`
	DateFormatted   string  `json:"date_formatted"` // Human-readable date
	MessageID       string  `json:"message_id"`
	HasAttach       bool    `json:"has_attachments"`
	Seen            bool    `json:"seen"`
	Flagged         bool    `json:"flagged"`
	SpamStatus      string  `json:"spam_status"`
	SpamScore       float64 `json:"spam_score"`
	SoftDeleted     bool    `json:"soft_deleted"`
	Type            string  `json:"type"`              // "email" or "calendar"
	CalendarEventID int64   `json:"calendar_event_id"` // 0 if not a calendar event
}

// SearchResult represents a search result
type SearchResult struct {
	Hits             []IndexDocument `json:"hits"`
	Query            string          `json:"query"`
	ProcessingTimeMs int64           `json:"processingTimeMs"`
	EstimatedTotal   int64           `json:"estimatedTotalHits"`
}

// request makes an HTTP request to Meilisearch
func (c *Client) request(method, path string, body interface{}) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal body: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, c.host+path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// Health checks if Meilisearch is available
func (c *Client) Health() error {
	_, err := c.request("GET", "/health", nil)
	return err
}

// CreateIndex creates the messages index with proper settings
func (c *Client) CreateIndex() error {
	// Create index
	indexBody := map[string]interface{}{
		"uid":        "messages",
		"primaryKey": "id",
	}
	_, err := c.request("POST", "/indexes", indexBody)
	if err != nil {
		// Index might already exist, ignore error
	}

	// Configure searchable attributes
	searchable := []string{
		"subject",
		"from",
		"to",
		"cc",
		"body",
		"message_id",
	}
	_, err = c.request("PUT", "/indexes/messages/settings/searchable-attributes", searchable)
	if err != nil {
		return fmt.Errorf("failed to set searchable attributes: %w", err)
	}

	// Configure filterable attributes
	filterable := []string{
		"user_id",
		"folder_id",
		"account_id",
		"seen",
		"flagged",
		"spam_status",
		"soft_deleted",
		"type",
		"date",
		"has_attachments",
	}
	_, err = c.request("PUT", "/indexes/messages/settings/filterable-attributes", filterable)
	if err != nil {
		return fmt.Errorf("failed to set filterable attributes: %w", err)
	}

	// Configure sortable attributes
	sortable := []string{
		"date",
		"spam_score",
	}
	_, err = c.request("PUT", "/indexes/messages/settings/sortable-attributes", sortable)
	if err != nil {
		return fmt.Errorf("failed to set sortable attributes: %w", err)
	}

	return nil
}

// IndexDocuments indexes multiple documents
func (c *Client) IndexDocuments(docs []IndexDocument) error {
	if len(docs) == 0 {
		return nil
	}
	_, err := c.request("POST", "/indexes/messages/documents", docs)
	return err
}

// DeleteDocument deletes a document by ID
func (c *Client) DeleteDocument(id int64) error {
	_, err := c.request("DELETE", fmt.Sprintf("/indexes/messages/documents/%d", id), nil)
	return err
}

// DeleteDocuments deletes multiple documents by IDs
func (c *Client) DeleteDocuments(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := c.request("POST", "/indexes/messages/documents/delete-batch", ids)
	return err
}

// Search performs a search query
func (c *Client) Search(userID int64, query string, limit, offset int) (*SearchResult, error) {
	searchReq := map[string]interface{}{
		"q":      query,
		"limit":  limit,
		"offset": offset,
		"filter": fmt.Sprintf("user_id = %d AND soft_deleted = false", userID),
		"sort":   []string{"date:desc"},
	}

	respBody, err := c.request("POST", "/indexes/messages/search", searchReq)
	if err != nil {
		return nil, err
	}

	var result SearchResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal search result: %w", err)
	}

	return &result, nil
}

// SearchInFolder performs a search within a specific folder
func (c *Client) SearchInFolder(userID, folderID int64, query string, limit, offset int) (*SearchResult, error) {
	searchReq := map[string]interface{}{
		"q":      query,
		"limit":  limit,
		"offset": offset,
		"filter": fmt.Sprintf("user_id = %d AND folder_id = %d AND soft_deleted = false", userID, folderID),
		"sort":   []string{"date:desc"},
	}

	respBody, err := c.request("POST", "/indexes/messages/search", searchReq)
	if err != nil {
		return nil, err
	}

	var result SearchResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal search result: %w", err)
	}

	return &result, nil
}

// GetStats returns index statistics
func (c *Client) GetStats() (map[string]interface{}, error) {
	respBody, err := c.request("GET", "/indexes/messages/stats", nil)
	if err != nil {
		return nil, err
	}

	var stats map[string]interface{}
	if err := json.Unmarshal(respBody, &stats); err != nil {
		return nil, fmt.Errorf("failed to unmarshal stats: %w", err)
	}

	return stats, nil
}
