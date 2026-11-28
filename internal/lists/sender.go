package lists

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/dkimsign"
	"github.com/ddletotam/ddmailserver/internal/logmask"
	"github.com/ddletotam/ddmailserver/internal/parser"
	smtpclient "github.com/ddletotam/ddmailserver/internal/smtp/client"
	"github.com/ddletotam/ddmailserver/internal/smtp/localdelivery"
)

// Store is what the sender needs from the database.
type Store interface {
	ClaimListDeliveries(limit int) ([]db.ListDelivery, error)
	ListIssueRaw(issueID int64) ([]byte, error)
	FinishListDelivery(d db.ListDelivery, sendErr error, permanent bool) error
}

// Route is where a list owner's letters leave from: source IP and its EHLO
// name; nil IP — the default route.
type Route func(ownerID int64) (net.IP, string)

// Sender sends the due list letters, a batch per Run.
type Sender struct {
	Store    Store
	Local    smtpclient.LocalDeliverer // our own domains; nil — everything via MX
	Signer   *dkimsign.Signer
	Hostname string
	Route    Route
	BaseURL  string
	BotName  string
	// External sends via MX (SendDirectEachVia; replaced in tests).
	External func(from string, to []string, msg []byte, helo string, ip net.IP) map[string]error

	mu      sync.Mutex
	running bool
}

const (
	batch   = 60 // letters per run
	workers = 4
)

// Run sends one batch; a run still going makes this one a no-op.
func (s *Sender) Run(ctx context.Context) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	ds, err := s.Store.ClaimListDeliveries(batch)
	if err != nil {
		log.Printf("lists: %v", err)
		return
	}
	if len(ds) == 0 {
		return
	}
	issues := map[int64][]byte{}
	for _, d := range ds {
		if d.IssueID == 0 || issues[d.IssueID] != nil {
			continue
		}
		raw, err := s.Store.ListIssueRaw(d.IssueID)
		if err != nil {
			log.Printf("lists: %v", err)
			continue
		}
		issues[d.IssueID] = raw
	}
	jobs := make(chan db.ListDelivery)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range jobs {
				err, permanent := s.send(d, issues[d.IssueID])
				if err != nil {
					log.Printf("lists: letter %d of %s to %s: %v (permanent=%v)", d.ID, d.ListAddr, logmask.Addr(d.Email), err, permanent)
				}
				if ferr := s.Store.FinishListDelivery(d, err, permanent); ferr != nil {
					log.Printf("lists: record letter %d: %v", d.ID, ferr)
				}
			}
		}()
	}
	for _, d := range ds {
		if ctx.Err() != nil {
			break // unsent ones come back after the lease
		}
		jobs <- d
	}
	close(jobs)
	wg.Wait()
	log.Printf("lists: %d letter(s) processed", len(ds))
}

func (s *Sender) send(d db.ListDelivery, issueRaw []byte) (error, bool) {
	l := Letter{ListAddr: d.ListAddr, ListTitle: d.ListTitle, To: d.Email, Token: d.Token,
		BaseURL: s.BaseURL, BotName: s.BotName, Now: time.Now()}
	var msg []byte
	if d.IssueID == 0 {
		msg = Confirmation(l)
	} else {
		if issueRaw == nil {
			return errors.New("issue not loaded"), false
		}
		var err error
		if msg, err = Issue(l, issueRaw); err != nil {
			return err, true
		}
	}
	msg = s.Signer.Sign(d.ListAddr, msg)

	if s.Local != nil {
		rcpt, err := s.Local.Lookup(d.Email)
		var reject *localdelivery.RejectError
		switch {
		case err == nil:
			parsed, perr := parser.New().ParseBytes(msg)
			if perr != nil {
				parsed = &parser.ParsedMessage{RawData: msg, RawSize: int64(len(msg))}
			}
			env := localdelivery.Envelope{From: d.ListAddr, Trusted: true}
			_, env.FromDomain, _ = strings.Cut(d.ListAddr, "@")
			res, errs := s.Local.Deliver(env, msg, parsed, []*localdelivery.Recipient{rcpt})
			if len(res) == 1 && res[0] == localdelivery.Failed {
				return fmt.Errorf("local delivery: %v", errs[0]), false
			}
			return nil, false
		case errors.As(err, &reject):
			return reject, true
		case !errors.Is(err, localdelivery.ErrNotLocal):
			return err, false
		}
	}

	ip, helo := net.IP(nil), s.Hostname
	if s.Route != nil {
		if rip, rhelo := s.Route(d.ListUserID); rip != nil && rhelo != "" {
			ip, helo = rip, rhelo
		}
	}
	send := s.External
	if send == nil {
		send = smtpclient.SendDirectEachVia
	}
	err := send(d.ListAddr, []string{d.Email}, msg, helo, ip)[d.Email]
	return err, smtpclient.IsPermanent(err)
}
