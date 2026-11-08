package web

import (
	"net/http/httptest"
	"testing"
)

func TestWSTokenPrefersHeader(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/desktop/v1/ws?token=fromquery", nil)
	r.Header.Set("Authorization", "Bearer fromheader")
	if got := wsToken(r); got != "fromheader" {
		t.Fatalf("header must win, got %q", got)
	}
}

func TestWSTokenFallsBackToQuery(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/desktop/v1/ws?token=fromquery", nil)
	if got := wsToken(r); got != "fromquery" {
		t.Fatalf("got %q", got)
	}
	r.Header.Set("Authorization", "Basic abc")
	if got := wsToken(r); got != "fromquery" {
		t.Fatalf("non-bearer header must be ignored, got %q", got)
	}
}

func TestWSTokenEmpty(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/desktop/v1/ws", nil)
	if got := wsToken(r); got != "" {
		t.Fatalf("got %q", got)
	}
}
