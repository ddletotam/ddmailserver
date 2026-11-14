package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/smtp/localdelivery"
	"github.com/gorilla/mux"
)

// Domain takeover (found 2026-11-14): registration is open, and any user
// could add any domain — the server then treated it as local, so mail other
// users sent to it was delivered into the claimant's mailboxes instead of
// going out through MX. These tests replay the attack against a real
// database: a claim must stay inert until ownership is proven.

// newDomainUser creates a user removed (with everything it owns) at cleanup.
func newDomainUser(t *testing.T, e *deliveryEnv, tag string) *models.User {
	t.Helper()
	u, err := e.db.CreateUser(fmt.Sprintf("%s%d", tag, time.Now().UnixNano()), "x", "", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM messages WHERE user_id = $1`,
			`DELETE FROM folders WHERE user_id = $1`,
			`DELETE FROM mailboxes WHERE user_id = $1`,
			`DELETE FROM domains WHERE user_id = $1`,
			`DELETE FROM users WHERE id = $1`,
		} {
			if _, err := e.db.Exec(q, u.ID); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	})
	return u
}

// as calls handler as user, returning the recorder (any status).
func as(user *models.User, handler http.HandlerFunc, method, path string, body interface{}, vars map[string]string) *httptest.ResponseRecorder {
	var payload string
	if body != nil {
		b, _ := json.Marshal(body)
		payload = string(b)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(setUserContext(req.Context(), user))
	if vars != nil {
		req = mux.SetURLVars(req, vars)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func createDomainAs(t *testing.T, e *deliveryEnv, user *models.User, name string) (int, DomainView) {
	t.Helper()
	rec := as(user, e.srv.HandleCreateDomain, "POST", "/api/domains", map[string]string{"domain": name}, nil)
	var v DomainView
	if rec.Code == http.StatusCreated {
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code, v
}

func inboxCount(t *testing.T, e *deliveryEnv, userID int64) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM messages m JOIN folders f ON f.id = m.folder_id
		WHERE m.user_id = $1 AND f.type = 'inbox'`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The attack itself: a fresh user claims a domain somebody else's mail goes
// to and plants a mailbox there. The victim's message must still leave via
// MX, and the attacker must receive nothing.
func TestDomainTakeover_ClaimDoesNotCaptureMail_DB(t *testing.T) {
	e := newDeliveryEnv(t)
	attacker := newDomainUser(t, e, "zzattacker")
	target := "corp-" + e.domain[:len(e.domain)-len(".test")] + ".example"
	victimAddr := "ceo@" + target

	code, view := createDomainAs(t, e, attacker, target)
	if code != http.StatusCreated {
		t.Fatalf("create domain: %d", code)
	}
	if view.Verified || view.Status != DomainStatusPending || view.Verification == nil ||
		view.Verification.Type != "TXT" || !strings.HasPrefix(view.Verification.Value, domainVerifyPrefix) {
		t.Fatalf("new domain %+v: want pending with a TXT record to publish", view)
	}

	// The mailbox API refuses a mailbox on an unverified domain…
	rec := as(attacker, e.srv.HandleCreateMailbox, "POST", "/api/mailboxes",
		map[string]interface{}{"domain_id": view.ID, "local_part": "ceo"}, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("mailbox on unverified domain: %d %s, want 409", rec.Code, rec.Body.String())
	}
	// …and even a mailbox that exists there (planted before the fix) is inert.
	if err := e.db.CreateMailbox(&models.Mailbox{UserID: attacker.ID, DomainID: view.ID, LocalPart: "ceo", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	if _, err := localdelivery.Lookup(e.db, victimAddr); !errors.Is(err, localdelivery.ErrNotLocal) {
		t.Fatalf("Lookup(%s) = %v, want ErrNotLocal", victimAddr, err)
	}

	outboxID, _ := e.send(e.me, victimAddr)
	var external []string
	if err := e.directTask(e.outboxRow(outboxID), func(a string) error {
		external = append(external, a)
		return nil
	}).Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The page shows the claim as pending, with the record to publish.
	page := as(attacker, e.srv.HandleDomainsPage, "GET", "/domains", nil, nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), view.Verification.Value) ||
		!strings.Contains(page.Body.String(), "/api/domains/"+fmt.Sprint(view.ID)+"/verify") {
		t.Fatalf("domains page: %d, want the TXT record and a Verify button", page.Code)
	}

	if len(external) != 1 || !strings.EqualFold(external[0], victimAddr) {
		t.Fatalf("sent through MX to %v, want [%s]", external, victimAddr)
	}
	if n := inboxCount(t, e, attacker.ID); n != 0 {
		t.Fatalf("attacker received %d message(s)", n)
	}

	// Nor can the attacker send as the planted address.
	ids, err := db.SenderIdentities(e.db, attacker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ids[victimAddr]; ok {
		t.Fatalf("%s is a sender identity of the claimant", victimAddr)
	}
}

// A pending claim does not lock the real owner out, and proof by TXT is
// what turns a domain local.
func TestDomainVerification_TXT_DB(t *testing.T) {
	e := newDeliveryEnv(t)
	squatter := newDomainUser(t, e, "zzsquat")
	owner := newDomainUser(t, e, "zzowner")
	name := "own-" + e.domain[:len(e.domain)-len(".test")] + ".example"

	if code, _ := createDomainAs(t, e, squatter, name); code != http.StatusCreated {
		t.Fatalf("squatter claim: %d", code)
	}
	code, view := createDomainAs(t, e, owner, name)
	if code != http.StatusCreated {
		t.Fatalf("owner claim blocked by a pending one: %d", code)
	}
	if code, _ := createDomainAs(t, e, owner, name); code != http.StatusConflict {
		t.Fatalf("second claim by the same user: %d, want 409", code)
	}

	var published []string
	e.srv.lookupTXT = func(_ context.Context, n string) ([]string, error) {
		if n != name {
			return nil, errors.New("NXDOMAIN")
		}
		return published, nil
	}
	verify := func(u *models.User, id int64) *httptest.ResponseRecorder {
		return as(u, e.srv.HandleVerifyDomain, "POST", "/api/domains/x/verify", nil,
			map[string]string{"id": fmt.Sprint(id)})
	}

	// No record (or someone else's token) — still pending.
	published = []string{"v=spf1 -all", domainVerifyPrefix + "0000"}
	if rec := verify(owner, view.ID); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), view.Verification.Value) {
		t.Fatalf("verify without record: %d %s", rec.Code, rec.Body.String())
	}
	if d, _ := e.db.GetDomainByID(view.ID); d.Verified {
		t.Fatal("verified without the record")
	}
	// Somebody else cannot verify the owner's claim.
	published = []string{view.Verification.Value}
	if rec := verify(squatter, view.ID); rec.Code != http.StatusNotFound {
		t.Fatalf("verify someone else's domain: %d", rec.Code)
	}

	if rec := verify(owner, view.ID); rec.Code != http.StatusOK {
		t.Fatalf("verify with record: %d %s", rec.Code, rec.Body.String())
	}
	if d, err := e.db.GetDomainByName(name); err != nil || d.UserID != owner.ID || d.VerifiedMethod != models.DomainVerifiedDNSTXT {
		t.Fatalf("GetDomainByName after verify: %+v, %v", d, err)
	}
	// The pending claim is gone, and nobody can claim it any more.
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM domains WHERE domain = $1`, name).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows for %s: %d (%v), want 1", name, n, err)
	}
	if code, _ := createDomainAs(t, e, squatter, name); code != http.StatusConflict {
		t.Fatalf("claim of a verified domain: %d, want 409", code)
	}

	// Now mailboxes work and receive mail.
	rec := as(owner, e.srv.HandleCreateMailbox, "POST", "/api/mailboxes",
		map[string]interface{}{"domain_id": view.ID, "local_part": "info"}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mailbox on verified domain: %d %s", rec.Code, rec.Body.String())
	}
	if r, err := localdelivery.Lookup(e.db, "info@"+name); err != nil || r.Mailbox.UserID != owner.ID {
		t.Fatalf("Lookup after verify: %+v, %v", r, err)
	}
}

// An administrator's domain is verified on creation.
func TestDomainCreate_AdminIsVerified_DB(t *testing.T) {
	e := newDeliveryEnv(t)
	admin := newDomainUser(t, e, "zzadmin")
	admin.IsAdminFlag = true
	code, view := createDomainAs(t, e, admin, "adm-"+e.domain[:len(e.domain)-len(".test")]+".example")
	if code != http.StatusCreated || !view.Verified || view.Status != DomainStatusActive ||
		view.VerifiedMethod != models.DomainVerifiedAdmin || view.Verification != nil {
		t.Fatalf("admin domain: %d %+v", code, view)
	}
}
