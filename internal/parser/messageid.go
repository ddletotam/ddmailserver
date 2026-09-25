package parser

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/mail"
	"strings"
)

// SyntheticMessageIDDomain is the right-hand side of every Message-ID the
// server derives itself. ".invalid" is reserved (RFC 2606), so a derived id
// can never collide with a real one and is recognisable at a glance.
const SyntheticMessageIDDomain = "ddmail.invalid"

// identityHeaders are the headers that name a message independently of the
// route it took. Transport headers (Received, Delivered-To, Return-Path,
// DKIM/ARC, X-*) are deliberately excluded: they differ between copies of the
// same message fetched from different upstream accounts.
var identityHeaders = []string{"Date", "From", "To", "Cc", "Subject"}

// DeriveMessageID computes a deterministic Message-ID for an upstream message
// that arrived without one. Identity is still (user_id, Message-ID); for such
// a message the id is derived from its content, never invented — the same
// message yields the same id on every sync, after an upstream MOVE, and after
// a UIDVALIDITY reset.
//
// The id hashes the identity headers plus the body. When the message has no
// Date header those fields are too weak to tell two messages apart, so the
// whole raw message is hashed instead. Returns "" for empty input.
func DeriveMessageID(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	h := sha256.New()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil || strings.TrimSpace(msg.Header.Get("Date")) == "" {
		h.Write([]byte("raw\x00"))
		h.Write(normalizeLineEndings(raw))
	} else {
		h.Write([]byte("hdr\x00"))
		for _, name := range identityHeaders {
			for _, v := range msg.Header[name] {
				writeField(h, name, strings.Join(strings.Fields(v), " "))
			}
		}
		body := new(bytes.Buffer)
		if _, err := body.ReadFrom(msg.Body); err != nil {
			h.Write([]byte("raw\x00"))
			h.Write(normalizeLineEndings(raw))
		} else {
			writeField(h, "body", string(bytes.TrimRight(normalizeLineEndings(body.Bytes()), " \t\n")))
		}
	}
	return "<noid." + hex.EncodeToString(h.Sum(nil))[:32] + "@" + SyntheticMessageIDDomain + ">"
}

// IsSyntheticMessageID reports whether id was produced by DeriveMessageID.
// Such ids mean nothing outside this server and must not leak into the
// In-Reply-To / References of outgoing mail.
func IsSyntheticMessageID(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	return strings.HasSuffix(strings.TrimSuffix(id, ">"), "@"+SyntheticMessageIDDomain)
}

// StripSyntheticMessageIDs drops derived ids from a whitespace-separated
// Message-ID list (an In-Reply-To or References value).
func StripSyntheticMessageIDs(list string) string {
	ids := strings.Fields(list)
	kept := ids[:0]
	for _, id := range ids {
		if !IsSyntheticMessageID(id) {
			kept = append(kept, id)
		}
	}
	return strings.Join(kept, " ")
}

// writeField length-prefixes each part so ("ab","c") and ("a","bc") hash
// differently.
func writeField(h interface{ Write([]byte) (int, error) }, name, value string) {
	var n [8]byte
	for _, s := range []string{name, value} {
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
}

func normalizeLineEndings(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}
