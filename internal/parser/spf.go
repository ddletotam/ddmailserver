package parser

import (
	"fmt"
	"net"
	"strings"
)

// SPFChecker performs SPF (Sender Policy Framework) verification
type SPFChecker struct{}

// NewSPFChecker creates a new SPF checker
func NewSPFChecker() *SPFChecker {
	return &SPFChecker{}
}

// CheckSPF verifies if the sender IP is authorized to send mail for the domain
// Returns AuthResult: pass, fail, softfail, neutral, none
func (c *SPFChecker) CheckSPF(senderIP, fromDomain string) (AuthResult, string) {
	if senderIP == "" || fromDomain == "" {
		return AuthResultNone, "missing sender IP or domain"
	}

	ip := net.ParseIP(senderIP)
	if ip == nil {
		return AuthResultNone, "invalid sender IP"
	}

	// Look up SPF record (TXT record)
	records, err := net.LookupTXT(fromDomain)
	if err != nil {
		return AuthResultNone, fmt.Sprintf("DNS lookup failed: %v", err)
	}

	// Find SPF record
	var spfRecord string
	for _, record := range records {
		if strings.HasPrefix(record, "v=spf1") {
			spfRecord = record
			break
		}
	}

	if spfRecord == "" {
		return AuthResultNone, "no SPF record found"
	}

	// Parse and evaluate SPF record
	return c.evaluateSPF(spfRecord, ip, fromDomain)
}

// evaluateSPF parses and evaluates an SPF record
func (c *SPFChecker) evaluateSPF(record string, ip net.IP, domain string) (AuthResult, string) {
	parts := strings.Fields(record)

	for _, part := range parts {
		// Skip version
		if part == "v=spf1" {
			continue
		}

		// Determine qualifier
		qualifier := "+"
		mechanism := part
		if len(part) > 0 && (part[0] == '+' || part[0] == '-' || part[0] == '~' || part[0] == '?') {
			qualifier = string(part[0])
			mechanism = part[1:]
		}

		// Check mechanism
		match, err := c.checkMechanism(mechanism, ip, domain)
		if err != nil {
			continue // Skip invalid mechanisms
		}

		if match {
			switch qualifier {
			case "+":
				return AuthResultPass, fmt.Sprintf("matched: %s", part)
			case "-":
				return AuthResultFail, fmt.Sprintf("matched: %s", part)
			case "~":
				return AuthResultSoftfail, fmt.Sprintf("matched: %s", part)
			case "?":
				return AuthResultNeutral, fmt.Sprintf("matched: %s", part)
			}
		}
	}

	// Default result if no mechanism matched
	return AuthResultNeutral, "no mechanism matched"
}

// checkMechanism evaluates a single SPF mechanism
func (c *SPFChecker) checkMechanism(mechanism string, ip net.IP, domain string) (bool, error) {
	// Handle "all" mechanism
	if mechanism == "all" {
		return true, nil
	}

	// Handle "a" mechanism (check A/AAAA records of domain)
	if mechanism == "a" || strings.HasPrefix(mechanism, "a:") {
		checkDomain := domain
		if strings.HasPrefix(mechanism, "a:") {
			checkDomain = mechanism[2:]
		}
		return c.checkA(checkDomain, ip)
	}

	// Handle "mx" mechanism (check MX records)
	if mechanism == "mx" || strings.HasPrefix(mechanism, "mx:") {
		checkDomain := domain
		if strings.HasPrefix(mechanism, "mx:") {
			checkDomain = mechanism[3:]
		}
		return c.checkMX(checkDomain, ip)
	}

	// Handle "ip4" mechanism
	if strings.HasPrefix(mechanism, "ip4:") {
		return c.checkIP4(mechanism[4:], ip)
	}

	// Handle "ip6" mechanism
	if strings.HasPrefix(mechanism, "ip6:") {
		return c.checkIP6(mechanism[4:], ip)
	}

	// Handle "include" mechanism (recursive SPF check)
	if strings.HasPrefix(mechanism, "include:") {
		includeDomain := mechanism[8:]
		result, _ := c.CheckSPF(ip.String(), includeDomain)
		return result == AuthResultPass, nil
	}

	// Handle "redirect" modifier
	if strings.HasPrefix(mechanism, "redirect=") {
		redirectDomain := mechanism[9:]
		result, _ := c.CheckSPF(ip.String(), redirectDomain)
		return result == AuthResultPass, nil
	}

	return false, fmt.Errorf("unknown mechanism: %s", mechanism)
}

// checkA checks if IP matches A/AAAA records
func (c *SPFChecker) checkA(domain string, ip net.IP) (bool, error) {
	addrs, err := net.LookupIP(domain)
	if err != nil {
		return false, err
	}

	for _, addr := range addrs {
		if addr.Equal(ip) {
			return true, nil
		}
	}
	return false, nil
}

// checkMX checks if IP matches MX records
func (c *SPFChecker) checkMX(domain string, ip net.IP) (bool, error) {
	mxRecords, err := net.LookupMX(domain)
	if err != nil {
		return false, err
	}

	for _, mx := range mxRecords {
		match, _ := c.checkA(mx.Host, ip)
		if match {
			return true, nil
		}
	}
	return false, nil
}

// checkIP4 checks if IP matches an IPv4 address or CIDR
func (c *SPFChecker) checkIP4(cidr string, ip net.IP) (bool, error) {
	// Check if it's a single IP or CIDR
	if !strings.Contains(cidr, "/") {
		cidr = cidr + "/32"
	}

	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		// Try as single IP
		checkIP := net.ParseIP(strings.TrimSuffix(cidr, "/32"))
		if checkIP != nil {
			return checkIP.Equal(ip), nil
		}
		return false, err
	}

	return network.Contains(ip), nil
}

// checkIP6 checks if IP matches an IPv6 address or CIDR
func (c *SPFChecker) checkIP6(cidr string, ip net.IP) (bool, error) {
	if !strings.Contains(cidr, "/") {
		cidr = cidr + "/128"
	}

	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		checkIP := net.ParseIP(strings.TrimSuffix(cidr, "/128"))
		if checkIP != nil {
			return checkIP.Equal(ip), nil
		}
		return false, err
	}

	return network.Contains(ip), nil
}
