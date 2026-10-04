package authfail

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type watched struct {
	client *http.Client
	watch  *HTTPWatch
	url    string
	hits   *int32
}

// newWatched serves status(method, path) for every request.
func newWatched(t *testing.T, status func(method, path string) int) watched {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(status(r.Method, r.URL.Path))
	}))
	t.Cleanup(srv.Close)
	w := &HTTPWatch{Base: srv.Client().Transport}
	return watched{client: &http.Client{Transport: w}, watch: w, url: srv.URL, hits: &hits}
}

func (w watched) do(t *testing.T, method, path string) error {
	t.Helper()
	req, err := http.NewRequest(method, w.url+path, strings.NewReader("<x/>"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := w.client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	return err
}

// TestHTTPWatch_401StopsTheSession: after the first 401 nothing else reaches
// the server — a push job with 50 queued changes must not turn into 50 failed
// logins — and the local failure is recognisable as a rejection.
func TestHTTPWatch_401StopsTheSession(t *testing.T) {
	w := newWatched(t, func(string, string) int { return http.StatusUnauthorized })

	if err := w.do(t, "REPORT", "/cal/"); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if !w.watch.Rejected() || w.watch.Accepted() {
		t.Fatalf("after 401: rejected=%v accepted=%v", w.watch.Rejected(), w.watch.Accepted())
	}
	for i := 0; i < 5; i++ {
		err := w.do(t, "PUT", "/cal/e.ics")
		if err == nil || !Is(err) {
			t.Fatalf("request %d after 401: err = %v, want a marked rejection", i, err)
		}
	}
	if n := atomic.LoadInt32(w.hits); n != 1 {
		t.Fatalf("server saw %d requests, want 1", n)
	}
}

// TestHTTPWatch_403OnWriteIsNotAboutThePassword: iCloud answers 403 to a PUT
// it will not store (a task into an event collection). That is the object's
// problem; the session that read everything fine is accepted.
func TestHTTPWatch_403OnWriteIsNotAboutThePassword(t *testing.T) {
	w := newWatched(t, func(method, _ string) int {
		if method == "PUT" {
			return http.StatusForbidden
		}
		return http.StatusMultiStatus
	})
	_ = w.do(t, "PROPFIND", "/")
	_ = w.do(t, "PUT", "/cal/task.ics")
	if w.watch.Rejected() {
		t.Fatal("403 on PUT counted as rejected credentials")
	}
	if !w.watch.Accepted() {
		t.Fatal("successful PROPFIND not counted as accepted")
	}
}

// TestHTTPWatch_403OnEveryRead: nothing readable at all — the credentials do
// not grant access (some servers answer a wrong app password this way).
func TestHTTPWatch_403OnEveryRead(t *testing.T) {
	w := newWatched(t, func(string, string) int { return http.StatusForbidden })
	_ = w.do(t, "PROPFIND", "/")
	_ = w.do(t, "REPORT", "/cal/")
	if !w.watch.Rejected() {
		t.Fatal("403 on every read not counted")
	}
}

// TestHTTPWatch_403OnOneCalendar: one shared calendar we may not read next to
// working ones is about that calendar, not the password.
func TestHTTPWatch_403OnOneCalendar(t *testing.T) {
	w := newWatched(t, func(_ string, path string) int {
		if path == "/shared/" {
			return http.StatusForbidden
		}
		return http.StatusMultiStatus
	})
	_ = w.do(t, "PROPFIND", "/")
	_ = w.do(t, "REPORT", "/shared/")
	_ = w.do(t, "REPORT", "/mine/")
	if w.watch.Rejected() || !w.watch.Accepted() {
		t.Fatalf("rejected=%v accepted=%v", w.watch.Rejected(), w.watch.Accepted())
	}
}

// TestHTTPWatch_NoAnswerNoVerdict: a server that never answered (5xx here)
// says nothing about the credentials either way.
func TestHTTPWatch_NoAnswerNoVerdict(t *testing.T) {
	w := newWatched(t, func(string, string) int { return http.StatusBadGateway })
	_ = w.do(t, "PROPFIND", "/")
	if w.watch.Rejected() || w.watch.Accepted() {
		t.Fatalf("rejected=%v accepted=%v", w.watch.Rejected(), w.watch.Accepted())
	}
	var nilWatch *HTTPWatch
	if nilWatch.Rejected() || nilWatch.Accepted() {
		t.Fatal("nil watch has a verdict")
	}
}
