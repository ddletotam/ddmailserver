// msgid-audit measures what the (user_id, Message-ID) identity costs: how many
// upstream messages share a Message-ID with a DIFFERENT message, so the sync
// takes them for an already-known one and never stores them.
//
// Read-only on both sides. It walks every enabled account exactly the way the
// sync does (same folders, BODY.PEEK[] — no \Seen is set), computes for each
// upstream message the id the sync would use (header or parser.DeriveMessageID)
// and a content fingerprint, and compares the fingerprints within each
// (user_id, Message-ID) group — upstream copies and the stored row alike.
//
// The fingerprint is parser.DeriveMessageID itself: a hash of
// Date/From/To/Cc/Subject + body that ignores Message-ID and transport headers,
// so the same message fetched from two accounts fingerprints the same.
//
// Output: a summary on stdout; per-conflict detail (subjects and addresses —
// personal data, 152-ФЗ) goes to -detail with mode 0600 and must stay on the
// server.
//
//	go run ./cmd/msgid-audit -config /etc/mailserver/config.yaml -detail /tmp/msgid-audit.tsv
package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"net/mail"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap"

	"github.com/ddletotam/ddmailserver/internal/config"
	"github.com/ddletotam/ddmailserver/internal/db"
	imapclient "github.com/ddletotam/ddmailserver/internal/imap/client"
	"github.com/ddletotam/ddmailserver/internal/parser"
)

// copyInfo is one sighting of a message: an upstream copy or the stored row.
type copyInfo struct {
	where   string // "acct:<email>/<folder>/<uid>" or "db:<row id>"
	fp      string
	date    string
	from    string
	subject string
}

type groupKey struct {
	user int64
	mid  string
}

func summarize(raw []byte) (date, from, subject string) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return "", "", ""
	}
	norm := func(v string) string { return strings.Join(strings.Fields(v), " ") }
	return norm(msg.Header.Get("Date")), norm(msg.Header.Get("From")), norm(msg.Header.Get("Subject"))
}

func main() {
	configPath := flag.String("config", "configs/config.yaml", "mailserver config")
	detailPath := flag.String("detail", "", "write per-conflict detail (TSV, mode 0600) here")
	findIDs := flag.String("find", "", "comma-separated Message-IDs: only report where each sits upstream (ALL folders, Trash/Junk included)")
	findAccount := flag.String("account", "", "with -find: only this account email")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	database, err := db.Connect(cfg.Database.GetDSN())
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer database.Close()
	database.SetEncryptionKey(cfg.Security.EncryptionKey)

	if *findIDs != "" {
		findUpstream(database, strings.Split(*findIDs, ","), *findAccount)
		return
	}

	groups := map[groupKey][]copyInfo{}
	stored := map[groupKey]bool{}
	noRaw := 0

	// 1) Stored rows: fingerprint of raw_email, streamed row by row.
	rows, err := database.Query(`SELECT id, user_id, message_id, raw_email FROM messages
		WHERE COALESCE(message_id, '') <> ''`)
	if err != nil {
		log.Fatalf("query messages: %v", err)
	}
	for rows.Next() {
		var id, user int64
		var mid string
		var raw []byte
		if err := rows.Scan(&id, &user, &mid, &raw); err != nil {
			log.Fatalf("scan: %v", err)
		}
		k := groupKey{user, mid}
		stored[k] = true
		if len(raw) == 0 {
			noRaw++
			continue
		}
		d, f, s := summarize(raw)
		groups[k] = append(groups[k], copyInfo{fmt.Sprintf("db:%d", id), parser.DeriveMessageID(raw), d, f, s})
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("rows: %v", err)
	}
	rows.Close()

	// 2) Upstream: every enabled account, the folders the sync pulls.
	accounts, err := database.GetAllEnabledAccounts()
	if err != nil {
		log.Fatalf("accounts: %v", err)
	}
	upstream, derived, failedAccounts := 0, 0, []string{}
	for _, acc := range accounts {
		t0 := time.Now()
		c, err := imapclient.New(acc)
		if err == nil {
			err = c.Connect()
		}
		if err != nil {
			failedAccounts = append(failedAccounts, fmt.Sprintf("%s (%v)", acc.Email, err))
			continue
		}
		mailboxes, err := c.ListFolders()
		if err != nil {
			failedAccounts = append(failedAccounts, fmt.Sprintf("%s (list: %v)", acc.Email, err))
			c.Disconnect()
			continue
		}
		n := 0
		for _, mb := range mailboxes {
			if !imapclient.SyncableMailbox(mb) {
				continue
			}
			status, err := c.SelectFolder(mb.Name)
			if err != nil || status.Messages == 0 {
				continue
			}
			uids := new(imap.SeqSet)
			uids.AddRange(1, 0)
			section := &imap.BodySectionName{Peek: true}
			msgs, done := c.FetchMessagesByUID(uids, []imap.FetchItem{imap.FetchEnvelope, imap.FetchUid, section.FetchItem()})
			for m := range msgs {
				var raw []byte
				for _, lit := range m.Body {
					if lit != nil {
						var buf bytes.Buffer
						_, _ = buf.ReadFrom(lit)
						raw = buf.Bytes()
					}
					break
				}
				if len(raw) == 0 || m.Envelope == nil {
					continue
				}
				mid := m.Envelope.MessageId
				if mid == "" {
					mid = parser.DeriveMessageID(raw)
					derived++
				}
				if mid == "" {
					continue
				}
				upstream++
				n++
				d, f, s := summarize(raw)
				k := groupKey{acc.UserID, mid}
				groups[k] = append(groups[k], copyInfo{
					fmt.Sprintf("acct:%s/%s/%d", acc.Email, mb.Name, m.Uid), parser.DeriveMessageID(raw), d, f, s,
				})
			}
			if err := <-done; err != nil {
				log.Printf("%s %q: fetch: %v", acc.Email, mb.Name, err)
			}
		}
		c.Disconnect()
		log.Printf("%s: %d messages in %s", acc.Email, n, time.Since(t0).Round(time.Second))
	}

	// 3) Conflicts: a group whose sightings carry more than one fingerprint.
	//    "different" — Date, From or Subject differ: certainly another message.
	//    "altered"   — those agree, only the rest differs: most likely the same
	//                  message rewritten on the way (list footer, re-encoding).
	var different, altered, lostDifferent int
	var detail []string
	keys := make([]groupKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].user != keys[j].user {
			return keys[i].user < keys[j].user
		}
		return keys[i].mid < keys[j].mid
	})
	for _, k := range keys {
		g := groups[k]
		fps := map[string]copyInfo{}
		for _, c := range g {
			if _, ok := fps[c.fp]; !ok {
				fps[c.fp] = c
			}
		}
		if len(fps) < 2 {
			continue
		}
		heads := map[string]bool{}
		for _, c := range fps {
			heads[c.date+"\x00"+c.from+"\x00"+c.subject] = true
		}
		kind := "altered"
		if len(heads) > 1 {
			kind = "different"
			different++
			// Variants beyond the one we keep are mail the user never got.
			lostDifferent += len(heads) - 1
		} else {
			altered++
		}
		for _, c := range g {
			detail = append(detail, strings.Join([]string{
				kind, fmt.Sprint(k.user), k.mid, c.where, c.fp, c.date, c.from, c.subject,
			}, "\t"))
		}
	}

	upstreamOnly := 0
	for k := range groups {
		if !stored[k] {
			upstreamOnly++
		}
	}

	fmt.Printf("stored rows with Message-ID: %d (без raw_email, не сравнить: %d)\n", len(stored), noRaw)
	fmt.Printf("upstream copies: %d (из них без Message-ID, id выведен: %d)\n", upstream, derived)
	fmt.Printf("ids seen upstream but not stored (удалены локально / ещё не засинкались): %d\n", upstreamOnly)
	fmt.Printf("conflicts — different messages under one id: %d groups, %d messages lost\n", different, lostDifferent)
	fmt.Printf("conflicts — same headers, content altered in transit: %d groups\n", altered)
	if len(failedAccounts) > 0 {
		fmt.Printf("accounts not audited: %d\n", len(failedAccounts))
		for _, a := range failedAccounts {
			fmt.Printf("  %s\n", a)
		}
	}
	if *detailPath != "" && len(detail) > 0 {
		body := "kind\tuser\tmessage_id\twhere\tfingerprint\tdate\tfrom\tsubject\n" + strings.Join(detail, "\n") + "\n"
		if err := os.WriteFile(*detailPath, []byte(body), 0o600); err != nil {
			log.Fatalf("detail: %v", err)
		}
		fmt.Printf("detail: %s (%d lines)\n", *detailPath, len(detail))
	}
}

// findUpstream answers «is this message at the source at all, and where?»:
// UID SEARCH HEADER Message-ID in EVERY mailbox, not only the ones the sync
// pulls — a message the sync never stored may sit in Trash, Junk or a folder
// a server-side rule moved it to.
func findUpstream(database *db.DB, ids []string, only string) {
	accounts, err := database.GetAllEnabledAccounts()
	if err != nil {
		log.Fatalf("accounts: %v", err)
	}
	for _, acc := range accounts {
		if only != "" && !strings.EqualFold(acc.Email, only) {
			continue
		}
		c, err := imapclient.New(acc)
		if err == nil {
			err = c.Connect()
		}
		if err != nil {
			fmt.Printf("%s: connect: %v\n", acc.Email, err)
			continue
		}
		mailboxes, err := c.ListFolders()
		if err != nil {
			fmt.Printf("%s: list: %v\n", acc.Email, err)
			c.Disconnect()
			continue
		}
		// Envelopes, not UID SEARCH HEADER: Yandex answers the latter with
		// «Backend error», which reads exactly like «not found».
		want := map[string]bool{}
		for _, id := range ids {
			want[strings.TrimSpace(id)] = true
		}
		seen := map[string]bool{}
		for _, mb := range mailboxes {
			status, err := c.SelectFolder(mb.Name)
			if err != nil || status.Messages == 0 {
				continue
			}
			uids := new(imap.SeqSet)
			uids.AddRange(1, 0)
			msgs, done := c.FetchMessagesByUID(uids, []imap.FetchItem{imap.FetchEnvelope, imap.FetchUid, imap.FetchFlags})
			for m := range msgs {
				if m.Envelope == nil || !want[m.Envelope.MessageId] {
					continue
				}
				seen[m.Envelope.MessageId] = true
				fmt.Printf("%s  %s  folder=%q uid=%d flags=%v syncable=%v\n",
					acc.Email, m.Envelope.MessageId, mb.Name, m.Uid, m.Flags, imapclient.SyncableMailbox(mb))
			}
			if err := <-done; err != nil {
				fmt.Printf("%s %q: fetch: %v\n", acc.Email, mb.Name, err)
			}
		}
		for id := range want {
			if !seen[id] {
				fmt.Printf("%s  %s  NOT FOUND in any of %d folders\n", acc.Email, id, len(mailboxes))
			}
		}
		c.Disconnect()
	}
}
