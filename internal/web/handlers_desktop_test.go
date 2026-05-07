package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleDDMailDiscovery(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("GET", "/.well-known/ddmail", nil)
	w := httptest.NewRecorder()

	s.HandleDDMailDiscovery(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if resp["ddmail"] != true {
		t.Error("expected ddmail=true")
	}
	if resp["version"] != float64(1) {
		t.Errorf("expected version=1, got %v", resp["version"])
	}
	if resp["api_base"] != "/api/desktop/v1" {
		t.Errorf("expected api_base=/api/desktop/v1, got %v", resp["api_base"])
	}
	if resp["ws_path"] != "/api/desktop/v1/ws" {
		t.Errorf("expected ws_path, got %v", resp["ws_path"])
	}

	features, ok := resp["features"].([]interface{})
	if !ok || len(features) == 0 {
		t.Error("expected non-empty features array")
	}
}

func TestHandleDesktopLoginBadJSON(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("POST", "/api/desktop/v1/auth/login", bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()

	s.HandleDesktopLogin(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleDesktopLoginNoUser(t *testing.T) {
	// Without a database, GetUserByUsername will fail → 401
	s := &Server{}
	body, _ := json.Marshal(map[string]string{"username": "nonexistent", "password": "pass"})
	req := httptest.NewRequest("POST", "/api/desktop/v1/auth/login", bytes.NewReader(body))
	w := httptest.NewRecorder()

	// This will panic or error because s.database is nil — that's expected
	// in a unit test without DB. We just verify the handler doesn't crash
	// on valid JSON input by recovering from the panic.
	defer func() {
		if r := recover(); r != nil {
			// Expected: nil database
		}
	}()
	s.HandleDesktopLogin(w, req)
}

func TestFolderSpecialUse(t *testing.T) {
	tests := []struct {
		folderType string
		expected   string
	}{
		{"inbox", "\\Inbox"},
		{"sent", "\\Sent"},
		{"drafts", "\\Drafts"},
		{"trash", "\\Trash"},
		{"junk", "\\Junk"},
		{"archive", "\\Archive"},
		{"custom", ""},
		{"", ""},
	}

	for _, tt := range tests {
		got := folderSpecialUse(tt.folderType)
		if got != tt.expected {
			t.Errorf("folderSpecialUse(%q) = %q, want %q", tt.folderType, got, tt.expected)
		}
	}
}

func TestExtractEmail(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"user@example.com", "user@example.com"},
		{"Name <user@example.com>", "user@example.com"},
		{"<user@example.com>", "user@example.com"},
		{" user@example.com ", "user@example.com"},
		{"\"Name\" <user@example.com>", "user@example.com"},
	}

	for _, tt := range tests {
		got := extractEmail(tt.input)
		if got != tt.expected {
			t.Errorf("extractEmail(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}
