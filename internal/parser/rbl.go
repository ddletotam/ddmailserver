package parser

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// RBLChecker performs DNSBL (DNS-based Blackhole List) lookups
type RBLChecker struct {
	lists   []RBLList
	timeout time.Duration
}

// RBLList represents a DNS blacklist
type RBLList struct {
	Name   string
	Zone   string
	Weight float64 // Score weight if listed
}

// RBLResult contains the result of an RBL check
type RBLResult struct {
	Listed   bool
	ListName string
	Response string
	Weight   float64
}

// DefaultRBLLists returns commonly used RBL lists
func DefaultRBLLists() []RBLList {
	return []RBLList{
		// Spamhaus - most reputable
		{Name: "Spamhaus ZEN", Zone: "zen.spamhaus.org", Weight: 5.0},
		// Spamcop
		{Name: "SpamCop", Zone: "bl.spamcop.net", Weight: 3.0},
		// Barracuda
		{Name: "Barracuda", Zone: "b.barracudacentral.org", Weight: 3.0},
		// SORBS
		{Name: "SORBS", Zone: "dnsbl.sorbs.net", Weight: 2.0},
		// UCEProtect Level 1
		{Name: "UCEProtect L1", Zone: "dnsbl-1.uceprotect.net", Weight: 2.0},
	}
}

// NewRBLChecker creates a new RBL checker with default lists
func NewRBLChecker() *RBLChecker {
	return &RBLChecker{
		lists:   DefaultRBLLists(),
		timeout: 5 * time.Second,
	}
}

// NewRBLCheckerWithLists creates an RBL checker with custom lists
func NewRBLCheckerWithLists(lists []RBLList) *RBLChecker {
	return &RBLChecker{
		lists:   lists,
		timeout: 5 * time.Second,
	}
}

// CheckIP checks if an IP is listed in any configured RBL
// Returns total score and list of results
func (c *RBLChecker) CheckIP(ipStr string) (float64, []RBLResult) {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return 0, nil
	}

	// Only check IPv4 for now (most RBLs don't support IPv6 well)
	ip4 := ip.To4()
	if ip4 == nil {
		return 0, nil
	}

	// Reverse the IP for DNSBL lookup
	reversed := c.reverseIP(ip4)

	// Check all lists concurrently
	var wg sync.WaitGroup
	resultsChan := make(chan RBLResult, len(c.lists))

	for _, list := range c.lists {
		wg.Add(1)
		go func(l RBLList) {
			defer wg.Done()
			result := c.checkList(reversed, l)
			resultsChan <- result
		}(list)
	}

	// Wait for all checks to complete
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	// Collect results
	var results []RBLResult
	var totalScore float64

	for result := range resultsChan {
		if result.Listed {
			results = append(results, result)
			totalScore += result.Weight
		}
	}

	return totalScore, results
}

// checkList performs a single RBL lookup
func (c *RBLChecker) checkList(reversedIP string, list RBLList) RBLResult {
	query := reversedIP + "." + list.Zone

	// Set a custom resolver with timeout
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: c.timeout}
			return d.DialContext(ctx, network, address)
		},
	}

	// Perform DNS lookup
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	addrs, err := resolver.LookupHost(ctx, query)
	if err != nil {
		// DNS error typically means not listed
		return RBLResult{
			Listed:   false,
			ListName: list.Name,
		}
	}

	if len(addrs) > 0 {
		return RBLResult{
			Listed:   true,
			ListName: list.Name,
			Response: addrs[0],
			Weight:   list.Weight,
		}
	}

	return RBLResult{
		Listed:   false,
		ListName: list.Name,
	}
}

// reverseIP reverses an IPv4 address for DNSBL lookup
// Example: 192.168.1.1 -> 1.1.168.192
func (c *RBLChecker) reverseIP(ip net.IP) string {
	ip4 := ip.To4()
	if ip4 == nil {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.%d", ip4[3], ip4[2], ip4[1], ip4[0])
}

// IsPrivateIP checks if an IP is private/internal (skip RBL checks for these)
func IsPrivateIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}

	// Check for private ranges
	privateRanges := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"::1/128",
		"fc00::/7",
	}

	for _, cidr := range privateRanges {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if network.Contains(ip) {
			return true
		}
	}

	return false
}

// FormatRBLResults formats RBL results for logging/storage
func FormatRBLResults(results []RBLResult) string {
	if len(results) == 0 {
		return ""
	}

	var parts []string
	for _, r := range results {
		parts = append(parts, fmt.Sprintf("%s (%s)", r.ListName, r.Response))
	}
	return strings.Join(parts, ", ")
}
