// Package signup holds the rules for logins chosen at self-service sign-up
// (the Telegram bot, docs/lunlun-bot-plan.md): what a login may look like and
// which names are kept back.
package signup

import (
	"crypto/rand"
	"math/big"
	"strings"
)

// Reasons a login is refused. The bot maps them to user-facing text.
const (
	ReasonInvalid  = "invalid"
	ReasonReserved = "reserved"
	ReasonTaken    = "taken"
)

const (
	MinLoginLen = 3
	MaxLoginLen = 32
)

// NormalizeLogin lower-cases and trims a login as typed; an address typed in
// full ("vasya@lunlun.email") keeps only its local part.
func NormalizeLogin(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[:i]
	}
	return s
}

// CheckLogin returns "" when the (normalized) login may be registered, or a
// Reason* constant. Whether it is free is the database's business (ReasonTaken).
func CheckLogin(login string) string {
	if len(login) < MinLoginLen || len(login) > MaxLoginLen {
		return ReasonInvalid
	}
	for i := 0; i < len(login); i++ {
		c := login[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '.' || c == '-' || c == '_':
			if i == 0 || i == len(login)-1 {
				return ReasonInvalid
			}
			if p := login[i-1]; p == '.' || p == '-' || p == '_' {
				return ReasonInvalid
			}
		default:
			return ReasonInvalid
		}
	}
	if login[0] < 'a' || login[0] > 'z' {
		return ReasonInvalid
	}
	if isReserved(login) {
		return ReasonReserved
	}
	return ""
}

// reservedExact are role and system addresses (RFC 2142 and common practice).
var reservedExact = map[string]bool{
	"abuse": true, "admin": true, "administrator": true, "billing": true, "help": true,
	"helpdesk": true, "hostmaster": true, "info": true, "mail": true, "mailer-daemon": true,
	"marketing": true, "news": true, "noc": true, "no-reply": true, "noreply": true,
	"office": true, "postmaster": true, "root": true, "sales": true, "security": true,
	"support": true, "sysadmin": true, "system": true, "team": true, "webmaster": true,
	"www": true, "ftp": true, "imap": true, "smtp": true, "pop": true, "mx": true,
	"dmarc": true, "dkim": true, "spf": true, "bounce": true, "bounces": true,
	"daemon": true, "nobody": true, "test": true, "service": true, "staff": true,
	"lunlun": true, "ddmail": true, "official": true, "moderator": true, "owner": true,
}

// reservedParts are names someone would use to pass for an organization —
// banks, payment systems, state services — anywhere in the login.
var reservedParts = []string{
	"lunlun", "ddmail", "postmaster", "administrator", "support", "security",
	"gosuslug", "nalog", "sberbank", "sber", "tinkoff", "tbank", "vtb", "alfabank",
	"gazprom", "raiffeisen", "sovcombank", "pochtabank", "otkritie", "rosbank",
	"paypal", "visa", "mastercard", "mir-pay", "qiwi", "yoomoney", "yandex",
	"apple", "google", "microsoft", "telegram", "whatsapp", "vkontakte", "mailru",
	"binance", "bybit", "coinbase", "wildberries", "ozon", "avito",
}

func isReserved(login string) bool {
	if reservedExact[login] {
		return true
	}
	for _, p := range reservedParts {
		if strings.Contains(login, p) {
			return true
		}
	}
	return false
}

// passwordAlphabet leaves out look-alikes (0/O, 1/l/I) — the password is
// read off a phone screen and typed into another device.
const passwordAlphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// NewPassword returns a random password of n characters.
func NewPassword(n int) (string, error) {
	b := make([]byte, n)
	max := big.NewInt(int64(len(passwordAlphabet)))
	for i := range b {
		k, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = passwordAlphabet[k.Int64()]
	}
	return string(b), nil
}
