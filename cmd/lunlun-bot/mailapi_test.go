package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMailAPINotices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret" || r.URL.Path != "/api/bot/v1/notices" {
			http.Error(w, `{"error":"unauthorized"}`, 401)
			return
		}
		_, _ = w.Write([]byte(`{"notices":[{"id":3,"tg_user_id":42,"kind":"k","text":"t"}]}`))
	}))
	defer srv.Close()
	ns, err := newMailAPI(srv.URL, "s3cret").notices(context.Background())
	if err != nil || len(ns) != 1 || ns[0].ID != 3 || ns[0].TgUserID != 42 {
		t.Fatalf("notices = %+v, %v", ns, err)
	}
	_, err = newMailAPI(srv.URL, "wrong").getAccount(context.Background(), 1)
	if errCode(err) != "unauthorized" {
		t.Fatalf("wrong secret: %v", err)
	}
}
