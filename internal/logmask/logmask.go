// Package logmask masks e-mail addresses for log output. Recipient (and
// sender) addresses are personal data under 152-ФЗ — logs keep only the
// first character of the local part and the full domain: "u***@gmail.com".
package logmask

import "strings"

// Addr masks a single address; tolerates "Name <addr>" and bare forms.
// Non-address strings come back unchanged.
func Addr(raw string) string {
	addr := strings.TrimSpace(raw)
	if i := strings.Index(addr, "<"); i != -1 {
		if j := strings.Index(addr[i:], ">"); j != -1 {
			addr = addr[i+1 : i+j]
		}
	}
	at := strings.LastIndexByte(addr, '@')
	if at <= 0 {
		return raw
	}
	return addr[:1] + "***@" + addr[at+1:]
}

// AddrList masks a comma/semicolon-separated address list.
func AddrList(raw string) string {
	parts := strings.FieldsFunc(raw, func(c rune) bool { return c == ',' || c == ';' })
	masked := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		masked = append(masked, Addr(p))
	}
	return strings.Join(masked, ", ")
}

// AddrSlice masks a slice of addresses.
func AddrSlice(addrs []string) string {
	masked := make([]string, 0, len(addrs))
	for _, a := range addrs {
		masked = append(masked, Addr(a))
	}
	return strings.Join(masked, ", ")
}
