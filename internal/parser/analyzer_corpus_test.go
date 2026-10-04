package parser

import (
	"strings"
	"testing"
)

// offlineAnalyzer is the stock analyzer without the network checks (SPF, DKIM,
// RBL), so the verdict is a pure function of the message.
func offlineAnalyzer() *Analyzer {
	cfg := DefaultAnalyzerConfig()
	cfg.CheckSPF = false
	cfg.CheckDKIM = false
	cfg.CheckRBL = false
	return NewAnalyzer(cfg)
}

func crlf(s string) string {
	return strings.ReplaceAll(strings.TrimLeft(s, "\n"), "\n", "\r\n")
}

// analyzerCorpus is a set of synthetic messages that together fire every
// analyzer rule. Domains are reserved example names.
func analyzerCorpus() map[string]string {
	return map[string]string{
		"clean": crlf(`
Received: from mx.example.org (mx.example.org [192.0.2.10]) by mail.example.net; Mon, 01 Jun 2026 10:00:05 +0000
Received: from client.example.org (client.example.org [192.0.2.20]) by mx.example.org; Mon, 01 Jun 2026 10:00:00 +0000
From: Alice <alice@example.org>
To: bob@example.net
Subject: Meeting notes
Date: Mon, 01 Jun 2026 10:00:00 +0000
Message-ID: <notes-1@example.org>
Content-Type: text/plain; charset=utf-8

Hi Bob, notes attached below. See https://example.org/notes
`),
		"promo": crlf(`
From: =?utf-8?B?0KHQsdC10YDQsdCw0L3Qug==?= <noreply@promo-sberr.example>
To: bob@example.net
Reply-To: claims@other.example
Subject: =?utf-8?B?8J+OgSDwn5SlIEZSRUUgTU9ORVkgV0lOTkVSISEh?=
X-Mailer: PHPMailer 6.0
Content-Type: text/html; charset=utf-8

<html><body><img src="a"><img src="b"><img src="c"><img src="d">
<p>Скидка! Получи бонус, заработок без вложений. Click here, act now.</p></body></html>
`),
		"phish": crlf(`
Received: from auth-xkfg-7.relay.example (unknown [198.51.100.7]) by mx.example.net; Mon, 01 Jun 2026 10:00:00 +0000
From: PayPal Support <security@gmail.com>
To: bob@example.net
Subject: Re: verify your account
Date: Mon, 01 Jun 2026 10:00:00 +0000
Message-ID: <p-1@gmail.com>
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="b1"

--b1
Content-Type: text/plain; charset=utf-8

Login now: https://paypa1-secure.example/login?token=QWxhZGRpbjpvcGVuIHNlc2FtZQ12345 and http://bit.ly/x1 http://bit.ly/x2
https://a.example/1 https://a.example/2 https://a.example/3 https://a.example/4
https://a.example/5 https://a.example/6 https://a.example/7 https://a.example/8
https://vk.cc/abc https://rusege-oleneva.example/c/91822064556776O6T6I132H9
--b1
Content-Type: application/octet-stream
Content-Disposition: attachment; filename="invoice.pdf.exe"

AAAA
--b1--
`),
		"scam-sender": crlf(`
Received: from a.example (a.example [192.0.2.1]) by b.example; Mon, 01 Jun 2026 10:00:00 +0000
Received: from c.example (c.example [192.0.2.2]) by a.example; Mon, 01 Jun 2026 11:00:00 +0000
From: =?utf-8?B?0JvQsNCx0L7RgNCw0YLQvtGA0LjRjyDQtNC+0YXQvtC00LA=?= <info@lab.example>
To: bob@example.net
Subject: =?utf-8?B?0KHQutC40LTQutCwIDUwJSDRgtC+0LvRjNC60L4g0YHQtdCz0L7QtNC90Y8=?=
Date: Mon, 01 Jun 2026 10:00:00 +0000
Message-ID: <s-1@lab.example>
Content-Type: text/plain; charset=utf-8

Срочно! Последний шанс.
`),
		"embedded": crlf(`
Received: from a.example (a.example [192.0.2.1]) by b.example; Mon, 01 Jun 2026 10:00:00 +0000
From: Fwd <fwd@example.org>
To: bob@example.net
Subject:
Date: Mon, 01 Jun 2026 10:00:00 +0000
Message-ID: <e-1@example.org>
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="b2"

--b2
Content-Type: text/plain

see attached
--b2
Content-Type: message/rfc822

From: inner@example.org
Subject: inner

inner body
--b2--
`),
	}
}

func parseCorpus(t *testing.T, raw string) *ParsedMessage {
	t.Helper()
	msg, err := New().ParseBytes([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return msg
}
