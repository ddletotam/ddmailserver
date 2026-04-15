package parser

import (
	"net/mail"
	"regexp"
	"strings"
	"time"
)

// ReceivedHop represents one hop in the Received header chain.
type ReceivedHop struct {
	From    string    // claimed HELO/EHLO name
	FromPTR string    // PTR-resolved name in parentheses
	FromIP  string    // IP address in brackets
	By      string    // receiving MTA
	With    string    // protocol (ESMTP, ESMTPS, etc.)
	For     string    // recipient address
	Date    time.Time // timestamp from the trailing ;date
	Raw     string    // raw header line
}

var (
	// from name (ptr_name [IP])  — IP may be IPv4 or IPv6
	// also: from name [IP] (no ptr)
	// also: from [IP]
	receivedFromPattern = regexp.MustCompile(`(?i)\bfrom\s+(?:([^\s\[(]+)\s+)?(?:\(\s*(?:([^\s\[\]()]+)\s+)?\[([0-9a-fA-F:.]+)\][^)]*\)|\[([0-9a-fA-F:.]+)\])`)
	// fallback: bare "from name" without ip/ptr
	receivedFromBarePattern = regexp.MustCompile(`(?i)\bfrom\s+([^\s(;]+)`)
	receivedByPattern       = regexp.MustCompile(`(?i)\bby\s+([^\s(;]+)`)
	receivedWithPattern     = regexp.MustCompile(`(?i)\bwith\s+(\S+)`)
	receivedForPattern      = regexp.MustCompile(`(?i)\bfor\s+<?([^>;\s]+)>?`)
)

// ParseReceivedHeader parses a single Received: header line.
func ParseReceivedHeader(line string) ReceivedHop {
	hop := ReceivedHop{Raw: line}

	// Date is after the last semicolon
	body := line
	if idx := strings.LastIndex(line, ";"); idx > 0 {
		dateStr := strings.TrimSpace(line[idx+1:])
		body = line[:idx]
		if t, err := mail.ParseDate(dateStr); err == nil {
			hop.Date = t
		}
	}

	if m := receivedFromPattern.FindStringSubmatch(body); m != nil {
		hop.From = m[1]
		hop.FromPTR = m[2]
		hop.FromIP = m[3]
		if hop.FromIP == "" {
			hop.FromIP = m[4]
		}
	} else if m := receivedFromBarePattern.FindStringSubmatch(body); m != nil {
		hop.From = m[1]
	}
	if m := receivedByPattern.FindStringSubmatch(body); m != nil {
		hop.By = m[1]
	}
	if m := receivedWithPattern.FindStringSubmatch(body); m != nil {
		hop.With = m[1]
	}
	if m := receivedForPattern.FindStringSubmatch(body); m != nil {
		hop.For = m[1]
	}

	return hop
}

// ParseReceivedChain parses all Received headers from a message.
// Returned slice is in the same order as headers: [0] = most recent (top of message),
// [last] = oldest hop (origin).
func ParseReceivedChain(rawHeaders map[string][]string) []ReceivedHop {
	var lines []string
	// Headers may be stored under different case keys
	for k, v := range rawHeaders {
		if strings.EqualFold(k, "Received") {
			lines = append(lines, v...)
		}
	}
	hops := make([]ReceivedHop, len(lines))
	for i, line := range lines {
		hops[i] = ParseReceivedHeader(line)
	}
	return hops
}

// ExtractOriginSenderIP returns the IP of the originating external sender by
// walking the Received chain from oldest to newest, returning the first
// non-private IP encountered.
func ExtractOriginSenderIP(hops []ReceivedHop) string {
	for i := len(hops) - 1; i >= 0; i-- {
		ip := hops[i].FromIP
		if ip == "" {
			continue
		}
		if IsPrivateIP(ip) {
			continue
		}
		return ip
	}
	return ""
}

// ExtractOriginHop returns the oldest hop with a non-private public IP.
func ExtractOriginHop(hops []ReceivedHop) *ReceivedHop {
	for i := len(hops) - 1; i >= 0; i-- {
		ip := hops[i].FromIP
		if ip == "" || IsPrivateIP(ip) {
			continue
		}
		return &hops[i]
	}
	return nil
}
