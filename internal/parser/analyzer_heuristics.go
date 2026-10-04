package parser

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Opt-in heuristics (categories subject_heuristics, sender_heuristics,
// link_heuristics; DefaultCategoryWeight = 0).
//
// They come from the former second spam engine of the web "why spam" page
// (internal/web/handlers_spam.go), which scored messages with its own rules
// and showed that score instead of the verdict a message was actually filtered
// by. That engine is gone; the rules it had and this analyzer did not are kept
// here so nothing is lost, but they stay off until a user gives the category a
// weight — turning them on by default would silently re-classify mail.

// Lookup tables of the heuristics. Read-only after init.
var (
	discountRe      = regexp.MustCompile(`\d+\s*%`)
	trackingParamRe = regexp.MustCompile(`[A-Za-z0-9+/]{20,}={0,2}`)
	trackingPathRe  = regexp.MustCompile(`/[a-zA-Z0-9]{15,}`)

	urgencyPatterns = []string{
		"act now", "urgent", "immediately", "expires", "last chance",
		"срочно", "немедленно", "истекает", "последний шанс",
	}

	// freeEmailProviders: a brand-like display name from one of these is a
	// classic phishing setup.
	freeEmailProviders = map[string]bool{
		"gmail.com": true, "yahoo.com": true, "hotmail.com": true, "outlook.com": true,
		"mail.ru": true, "yandex.ru": true, "rambler.ru": true, "bk.ru": true,
		"inbox.ru": true, "list.ru": true, "aol.com": true, "protonmail.com": true,
		"icloud.com": true, "me.com": true, "live.com": true, "msn.com": true,
	}

	suspiciousBrandNames = []string{
		"paypal", "amazon", "apple", "microsoft", "google", "facebook", "instagram",
		"netflix", "bank", "visa", "mastercard", "support", "security", "admin",
		"service", "account", "verify", "update", "confirm", "sberbank", "tinkoff",
		"vtb", "alfa-bank", "gazprom",
	}

	// regionalShorteners are shorteners not in the stock URLShorteners list.
	regionalShorteners = []string{"clck.ru", "vk.cc"}

	commonDomainWords = map[string]bool{
		"shop": true, "store": true, "mail": true, "web": true, "info": true,
		"online": true, "digital": true, "tech": true, "cloud": true, "data": true,
		"help": true, "support": true, "sales": true, "news": true, "blog": true,
	}

	credentialURLWords = []string{"login", "signin", "verify", "account", "secure", "update"}
)

// analyzeSubjectHeuristics: discount bait, empty or fake-reply subjects,
// Cyrillic ALL CAPS, urgency wording.
func (a *Analyzer) analyzeSubjectHeuristics(msg *ParsedMessage) ruleHits {
	var hits ruleHits
	subject := msg.Subject

	if strings.TrimSpace(subject) == "" {
		hits.add(0.5, "empty subject")
	}
	if discountRe.MatchString(subject) {
		hits.add(1.0, "discount percentage in subject")
	}
	if (strings.HasPrefix(subject, "Re:") || strings.HasPrefix(subject, "Fwd:")) && msg.InReplyTo == "" {
		hits.add(0.5, "reply/forward subject without In-Reply-To")
	}
	if cyrillicCapsSubject(subject) {
		hits.add(1.0, "excessive caps in subject (Cyrillic)")
	}

	content := strings.ToLower(subject + " " + msg.Body + " " + stripHTML(msg.BodyHTML))
	for _, p := range urgencyPatterns {
		if strings.Contains(content, p) {
			hits.add(0.5, "urgency: "+p)
			break
		}
	}
	return hits
}

// cyrillicCapsSubject reports a mostly-uppercase subject that the stock
// content check misses: that one counts only Latin capitals against the byte
// length, so an all-caps Russian subject never trips it.
func cyrillicCapsSubject(subject string) bool {
	runes := []rune(subject)
	if len(runes) <= 10 {
		return false
	}
	upper, cyrillic := 0, 0
	for _, r := range runes {
		if unicode.Is(unicode.Cyrillic, r) {
			cyrillic++
		}
		if unicode.IsUpper(r) {
			upper++
		}
	}
	return cyrillic > 0 && float64(upper)/float64(len(runes)) > 0.5
}

// analyzeSenderHeuristics: brand-like name from free mail, a random-looking
// sender domain, another address hidden in the display name.
func (a *Analyzer) analyzeSenderHeuristics(msg *ParsedMessage) ruleHits {
	var hits ruleHits
	if msg.From == nil {
		return nil
	}
	name := strings.ToLower(msg.From.Name)
	address := strings.ToLower(msg.From.Address)
	domain := extractDomain(address)

	if freeEmailProviders[domain] {
		for _, brand := range suspiciousBrandNames {
			if strings.Contains(name, brand) {
				hits.add(3.0, fmt.Sprintf("brand-like name %q from free mail provider %s", msg.From.Name, domain))
				break
			}
		}
	}
	if isRandomDomain(domain) {
		hits.add(2.0, "random-looking sender domain: "+domain)
	}
	if strings.Contains(msg.From.Name, "@") {
		if other := addressInDisplayName(msg.From.Name); other != "" && other != address {
			hits.add(2.0, "display name contains another address: "+msg.From.Name)
		}
	}
	return hits
}

// addressInDisplayName returns the address a display name pretends to be:
// the part in <...>, or the whole name.
func addressInDisplayName(name string) string {
	name = strings.TrimSpace(name)
	if start, end := strings.Index(name, "<"), strings.Index(name, ">"); start >= 0 && end > start {
		return strings.ToLower(strings.TrimSpace(name[start+1 : end]))
	}
	return strings.ToLower(name)
}

// LinkReport classifies the links of a message for display.
type LinkReport struct {
	URLs []string
	// Shorteners are links through a URL shortener (stock or regional list).
	Shorteners []string
	// Suspicious are login/verify-style links to a domain that is neither
	// the sender's nor a known brand's.
	Suspicious []string
}

// Links classifies the message's links with this analyzer's shortener list.
func (a *Analyzer) Links(msg *ParsedMessage) LinkReport {
	var rep LinkReport
	rep.URLs = extractURLs(msg.Body + " " + msg.BodyHTML)
	shorteners := append(append([]string{}, a.config.URLShorteners...), regionalShorteners...)
	senderDomain := ""
	if msg.From != nil {
		senderDomain = extractDomain(msg.From.Address)
	}
	for _, u := range rep.URLs {
		lower := strings.ToLower(u)
		for _, s := range shorteners {
			if strings.Contains(lower, s) {
				rep.Shorteners = append(rep.Shorteners, u)
				break
			}
		}
		if isCredentialLinkToStranger(lower, senderDomain) {
			rep.Suspicious = append(rep.Suspicious, u)
		}
	}
	return rep
}

func isCredentialLinkToStranger(lowerURL, senderDomain string) bool {
	for _, w := range credentialURLWords {
		if strings.Contains(lowerURL, w) {
			return !strings.Contains(lowerURL, senderDomain) && !isKnownBrandDomain(urlHost(lowerURL))
		}
	}
	return false
}

// analyzeLinkHeuristics: regional shorteners, credential links to strangers,
// tracking redirects, random-looking link domains, links that do not belong to
// the brand the sender name claims.
func (a *Analyzer) analyzeLinkHeuristics(msg *ParsedMessage) ruleHits {
	var hits ruleHits
	urls := extractURLs(msg.Body + " " + msg.BodyHTML)
	senderDomain, senderName := "", ""
	if msg.From != nil {
		senderDomain = extractDomain(msg.From.Address)
		senderName = strings.ToLower(msg.From.Name)
	}
	senderIsBrand := isKnownBrandDomain(senderDomain)
	claimedBrand := claimedBrand(senderName)

	var regional, credential, tracking, random, mismatch int
	for _, u := range urls {
		lower := strings.ToLower(u)
		host := urlHost(lower)
		for _, s := range regionalShorteners {
			if strings.Contains(lower, s) {
				regional++
				break
			}
		}
		if isCredentialLinkToStranger(lower, senderDomain) {
			credential++
		}
		if isTrackingURL(u) && !isKnownBrandDomain(host) {
			tracking++
		}
		if host != "" && !isKnownBrandDomain(host) && isRandomDomain(host) {
			random++
		}
		if claimedBrand != "" && !senderIsBrand && host != "" && host != senderDomain &&
			!domainIn(host, knownBrands[claimedBrand]) {
			mismatch++
		}
	}

	capped := func(n int, each, limit float64) float64 {
		s := float64(n) * each
		if s > limit {
			return limit
		}
		return s
	}
	if regional > 0 {
		hits.add(capped(regional, 0.5, 2.0), fmt.Sprintf("regional URL shortener(s): %d", regional))
	}
	if credential > 0 {
		hits.add(capped(credential, 1.5, 4.0), fmt.Sprintf("login/verify links to foreign domains: %d", credential))
	}
	if tracking > 0 {
		hits.add(capped(tracking, 1.0, 3.0), fmt.Sprintf("tracking links with encoded parameters: %d", tracking))
	}
	if random > 0 {
		hits.add(capped(random, 1.5, 3.0), fmt.Sprintf("links to random-looking domains: %d", random))
	}
	if mismatch > 0 {
		hits.add(capped(mismatch, 2.0, 4.0), fmt.Sprintf("links not matching the claimed brand %q: %d", claimedBrand, mismatch))
	}
	return hits
}

// claimedBrand returns the first known brand (in name order, so the result is
// deterministic) the display name mentions, or "".
func claimedBrand(lowerName string) string {
	if lowerName == "" {
		return ""
	}
	brands := make([]string, 0, len(knownBrands))
	for b := range knownBrands {
		brands = append(brands, b)
	}
	sort.Strings(brands)
	for _, b := range brands {
		if strings.Contains(lowerName, b) {
			return b
		}
	}
	return ""
}

func domainIn(domain string, legit []string) bool {
	for _, d := range legit {
		if domain == d || strings.HasSuffix(domain, "."+d) {
			return true
		}
	}
	return false
}

// isKnownBrandDomain reports whether domain belongs to a known brand.
func isKnownBrandDomain(domain string) bool {
	domain = strings.ToLower(domain)
	if domain == "" {
		return false
	}
	for _, legit := range knownBrands {
		if domainIn(domain, legit) {
			return true
		}
	}
	return false
}

// isRandomDomain reports a domain that looks generated: a long name with
// almost no vowels, or several non-word hyphenated chunks ("rusege-oleneva").
func isRandomDomain(domain string) bool {
	parts := strings.Split(domain, ".")
	if len(parts) < 2 {
		return false
	}
	name := parts[0]
	if len(parts) > 2 {
		name = parts[len(parts)-2]
	}

	if len(name) > 12 {
		vowels, consonants := 0, 0
		for _, r := range strings.ToLower(name) {
			switch {
			case r == 'a' || r == 'e' || r == 'i' || r == 'o' || r == 'u':
				vowels++
			case r >= 'a' && r <= 'z':
				consonants++
			}
		}
		if vowels > 0 && consonants > 0 && float64(vowels)/float64(vowels+consonants) < 0.15 {
			return true
		}
	}

	if strings.Contains(name, "-") {
		randomLooking := 0
		for _, part := range strings.Split(name, "-") {
			if len(part) >= 4 && len(part) <= 8 && !commonDomainWords[strings.ToLower(part)] {
				randomLooking++
			}
		}
		if randomLooking >= 2 {
			return true
		}
	}
	return false
}

// isTrackingURL reports a redirect/tracking link: a base64-ish blob in the
// query or a long opaque token in the path.
func isTrackingURL(u string) bool {
	if idx := strings.Index(u, "?"); idx != -1 && trackingParamRe.MatchString(u[idx+1:]) {
		return true
	}
	return trackingPathRe.MatchString(u)
}

// urlHost extracts the lower-cased host of an http(s) URL without a port.
func urlHost(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if idx := strings.IndexAny(u, "/?#"); idx != -1 {
		u = u[:idx]
	}
	if idx := strings.Index(u, ":"); idx != -1 {
		u = u[:idx]
	}
	return strings.ToLower(u)
}
