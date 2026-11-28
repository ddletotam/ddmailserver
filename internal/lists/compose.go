// Package lists sends mailing-list letters (docs/lunlun-bot-plan.md,
// «Рассылки»): a personal copy of the owner's letter to every subscriber,
// with a one-click unsubscribe, and the confirmation requests.
package lists

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"strings"
	"time"

	"github.com/ddletotam/ddmailserver/internal/outbox"
)

// Letter is what one delivery needs to be put together.
type Letter struct {
	ListAddr  string
	ListTitle string
	To        string
	Token     string // the subscriber's link token
	BaseURL   string // https://mail.lunlun.email
	BotName   string // robotohobot
	Now       time.Time
}

func (l Letter) unsubscribeURL() string { return l.BaseURL + "/l/u/" + l.Token }
func (l Letter) confirmURL() string     { return l.BaseURL + "/l/c/" + l.Token }

func (l Letter) domain() string {
	_, d, _ := strings.Cut(l.ListAddr, "@")
	return d
}

func messageID(domain string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "<list." + hex.EncodeToString(b) + "@" + domain + ">"
}

func (l Letter) commonHeaders(w *strings.Builder, subject string) {
	from := mail.Address{Name: l.ListTitle, Address: l.ListAddr}
	fmt.Fprintf(w, "From: %s\r\n", from.String())
	fmt.Fprintf(w, "To: <%s>\r\n", l.To)
	fmt.Fprintf(w, "Subject: %s\r\n", subject)
	fmt.Fprintf(w, "Date: %s\r\n", l.Now.Format(time.RFC1123Z))
	fmt.Fprintf(w, "Message-ID: %s\r\n", messageID(l.domain()))
	w.WriteString("MIME-Version: 1.0\r\n")
}

// Confirmation is the letter asking to confirm a subscription.
func Confirmation(l Letter) []byte {
	var w strings.Builder
	l.commonHeaders(&w, mime.BEncoding.Encode("utf-8", "Подтвердите подписку на рассылку «"+l.ListTitle+"»"))
	w.WriteString("Auto-Submitted: auto-generated\r\n")
	outbox.WriteTextPart(&w, "text/plain", fmt.Sprintf(
		"Здравствуйте!\n\n"+
			"Через Telegram-бота @%s попросили подписать адрес %s на рассылку «%s» (%s).\n\n"+
			"Подтвердить подписку:\n%s\n\n"+
			"Если вы не подписывались — ничего не делайте: без подтверждения писем рассылки не будет.\n",
		l.BotName, l.To, l.ListTitle, l.ListAddr, l.confirmURL()))
	return []byte(w.String())
}

// Issue is the owner's letter as it goes to one subscriber: from the list,
// replies to the owner, the owner's content intact and a footer with the
// unsubscribe link after it; List-Unsubscribe for the one-click button of
// mail services (RFC 8058).
func Issue(l Letter, raw []byte) ([]byte, error) {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("owner's letter: %w", err)
	}
	body, err := io.ReadAll(m.Body)
	if err != nil {
		return nil, err
	}
	var w strings.Builder
	subject := m.Header.Get("Subject")
	l.commonHeaders(&w, subject)
	if from := m.Header.Get("From"); from != "" {
		fmt.Fprintf(&w, "Reply-To: %s\r\n", from)
	}
	fmt.Fprintf(&w, "List-Id: %s <%s>\r\n", mime.QEncoding.Encode("utf-8", l.ListTitle), strings.Replace(l.ListAddr, "@", ".", 1))
	fmt.Fprintf(&w, "List-Unsubscribe: <%s>\r\n", l.unsubscribeURL())
	w.WriteString("List-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n")
	w.WriteString("Precedence: bulk\r\n")
	boundary := "lunlun-list-" + hex.EncodeToString([]byte(l.Token))[:16]
	fmt.Fprintf(&w, "Content-Type: multipart/mixed; boundary=\"%s\"\r\n\r\n", boundary)

	fmt.Fprintf(&w, "--%s\r\n", boundary)
	ct := m.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/plain; charset=us-ascii"
	}
	fmt.Fprintf(&w, "Content-Type: %s\r\n", ct)
	if cte := m.Header.Get("Content-Transfer-Encoding"); cte != "" {
		fmt.Fprintf(&w, "Content-Transfer-Encoding: %s\r\n", cte)
	}
	w.WriteString("\r\n")
	w.Write(body)
	if !bytes.HasSuffix(body, []byte("\n")) {
		w.WriteString("\r\n")
	}
	fmt.Fprintf(&w, "--%s\r\n", boundary)
	outbox.WriteTextPart(&w, "text/plain", fmt.Sprintf(
		"—\nВы получили это письмо, потому что подписались на рассылку «%s» через Telegram-бота @%s.\n"+
			"Отписаться: %s\n", l.ListTitle, l.BotName, l.unsubscribeURL()))
	fmt.Fprintf(&w, "--%s--\r\n", boundary)
	return []byte(w.String()), nil
}
