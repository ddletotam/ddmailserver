package authfail

import (
	"fmt"
	"net/http"
	"sync"
)

// HTTPWatch is an http.RoundTripper for the CalDAV/CardDAV clients that
// watches response statuses for the verdict on the credentials.
//
// Reading the status on the wire, instead of parsing error texts afterwards,
// is the only way to cover every request: discovery, REPORTs per calendar,
// PUT/DELETE pushes, the fallback requests made outside go-webdav — each
// formats its errors differently, and some 401s (a per-calendar REPORT) never
// even fail the whole job, they become one line in "partial sync errors".
//
// After the first 401 every further request of this client fails locally
// without being sent: the server has told us the password is wrong, and a
// push job with 50 queued changes would otherwise collect 50 more refusals in
// one run.
//
// 403 counts against the credentials only on reads (PROPFIND/REPORT/GET) and
// only when nothing in the session succeeded: a 403 on a PUT is the server
// refusing that object (iCloud on a task in an event calendar), and a 403 on
// one shared calendar next to working ones is about that calendar.
type HTTPWatch struct {
	Base http.RoundTripper

	mu            sync.Mutex
	unauthorized  bool
	forbiddenRead bool
	ok            bool
}

// RoundTrip implements http.RoundTripper.
func (w *HTTPWatch) RoundTrip(req *http.Request) (*http.Response, error) {
	w.mu.Lock()
	dead := w.unauthorized
	w.mu.Unlock()
	if dead {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, Mark(fmt.Errorf("%s not sent: the server already rejected the credentials (401) in this session", req.Method))
	}

	base := w.Base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	w.mu.Lock()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		w.unauthorized = true
	case resp.StatusCode == http.StatusForbidden && isRead(req.Method):
		w.forbiddenRead = true
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		w.ok = true
	}
	w.mu.Unlock()
	return resp, nil
}

func isRead(method string) bool {
	switch method {
	case "PROPFIND", "REPORT", http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// Rejected reports whether the server rejected the credentials in this
// session.
func (w *HTTPWatch) Rejected() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.unauthorized || (w.forbiddenRead && !w.ok)
}

// Accepted reports whether the server accepted the credentials: at least one
// successful response and no 401.
func (w *HTTPWatch) Accepted() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ok && !w.unauthorized
}

// ReportTo turns the session's verdict into a guard report: rejected →
// Rejected with cause (marked, so callers can test it with Is), accepted →
// Accepted, neither (network trouble before any answer) → nothing. Returns
// true when the credentials were rejected.
func (w *HTTPWatch) ReportTo(g *Guard, s Subject, cause error) bool {
	switch {
	case w.Rejected():
		// The job's own error may be anything ("partial sync errors: …");
		// what the user needs to read in last_error is the verdict.
		if cause == nil || !Is(cause) {
			cause = w.verdictError()
		}
		g.Rejected(s, cause)
		return true
	case w.Accepted():
		g.Accepted(s)
	}
	return false
}

func (w *HTTPWatch) verdictError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.unauthorized {
		return Mark(fmt.Errorf("server rejected the credentials (HTTP 401 Unauthorized)"))
	}
	return Mark(fmt.Errorf("server refused every read with HTTP 403 Forbidden"))
}
