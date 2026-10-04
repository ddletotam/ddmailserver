package parser

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// AnalyzerConfig contains configuration for the spam analyzer
type AnalyzerConfig struct {
	Enabled             bool
	SuspiciousThreshold float64 // score >= threshold -> suspicious
	SpamThreshold       float64 // score >= threshold -> spam
	CheckHeaders        bool
	CheckContent        bool
	CheckAttachments    bool
	CheckLinks          bool
	CheckSPF            bool
	CheckDKIM           bool
	CheckRBL            bool
	DangerousExtensions []string
	SpamWords           []string
	URLShorteners       []string
	RBLLists            []RBLList
}

// DefaultAnalyzerConfig returns a sensible default configuration
func DefaultAnalyzerConfig() *AnalyzerConfig {
	return &AnalyzerConfig{
		Enabled:             true,
		SuspiciousThreshold: 3.0,
		SpamThreshold:       6.0,
		CheckHeaders:        true,
		CheckContent:        true,
		CheckAttachments:    true,
		CheckLinks:          true,
		CheckSPF:            true,
		CheckDKIM:           true,
		CheckRBL:            true,
		DangerousExtensions: DangerousExtensions,
		SpamWords: []string{
			// English
			"viagra", "cialis", "lottery", "winner", "nigerian prince",
			"free money", "act now", "limited time", "click here",
			"unsubscribe", "you have been selected", "congratulations",
			"100% free", "no cost", "risk free", "guaranteed",
			// Russian - sales/marketing
			"распродажа", "купон", "скидка", "акция", "бесплатн", "выигр", "подарок",
			"персональн", "эксклюзив", "только для вас", "только сегодня", "последний шанс",
			"активир", "получи", "забери", "успей", "торопись", "не упусти",
			// Russian - financial scams
			"заработ", "доход", "инвести", "прибыль", "без вложений", "пассивн",
			"кредит", "займ", "одобрен", "микрозайм", "долг", "избавиться от",
			"финансов", "капитал", "бонус на баланс", "приветственный бонус",
			"приглашение в клуб", "закрытый клуб", "vip", "элитн",
			// Russian - crypto/trading scams
			"криптовалют", "биткоин", "трейдинг", "торговл", "сигнал",
			"стратеги", "робот", "автоматическ",
			// Generic spam
			"работа на дому", "казино", "ставки",
		},
		URLShorteners: []string{
			"bit.ly", "tinyurl.com", "t.co", "goo.gl", "ow.ly",
			"is.gd", "buff.ly", "adf.ly", "bl.ink", "lnkd.in",
			"shorturl.at", "cutt.ly",
		},
	}
}

// Analyzer performs spam analysis on parsed messages
type Analyzer struct {
	config      *AnalyzerConfig
	spfChecker  *SPFChecker
	dkimChecker *DKIMChecker
	rblChecker  *RBLChecker
}

// NewAnalyzer creates a new spam analyzer
func NewAnalyzer(config *AnalyzerConfig) *Analyzer {
	if config == nil {
		config = DefaultAnalyzerConfig()
	}

	analyzer := &Analyzer{config: config}

	if config.CheckSPF {
		analyzer.spfChecker = NewSPFChecker()
	}
	if config.CheckDKIM {
		analyzer.dkimChecker = NewDKIMChecker()
	}
	if config.CheckRBL {
		if len(config.RBLLists) > 0 {
			analyzer.rblChecker = NewRBLCheckerWithLists(config.RBLLists)
		} else {
			analyzer.rblChecker = NewRBLChecker()
		}
	}

	return analyzer
}

// Analyze performs spam analysis on a parsed message (without network checks)
func (a *Analyzer) Analyze(msg *ParsedMessage) {
	a.AnalyzeWithContext(msg, "", "")
}

// AnalyzeWithContext performs full spam analysis including network checks
// senderIP is the connecting client IP, fromDomain is extracted from MAIL FROM
func (a *Analyzer) AnalyzeWithContext(msg *ParsedMessage, senderIP, fromDomain string) {
	a.AnalyzeWithDisabledChecks(msg, senderIP, fromDomain, nil)
}

// AnalyzeWithDisabledChecks performs spam analysis with user-disabled checks
// disabledChecks is a map of check names that should be skipped for this user
// Check names: "spf", "dkim", "rbl", "url_shortener", "spam_word:xxx"
func (a *Analyzer) AnalyzeWithDisabledChecks(msg *ParsedMessage, senderIP, fromDomain string, disabledChecks map[string]bool) {
	a.AnalyzeWithUserConfig(msg, senderIP, fromDomain, disabledChecks, nil)
}

// SpamCheckCategories is the canonical list of named check categories the
// analyzer accumulates score into. Each name is what UIs and DB rows use:
// keep this in sync with `disabledChecks` keys + weight columns.
//
// The last three are opt-in (DefaultCategoryWeight = 0): heuristics ported
// from the former second engine of the web "why spam" page. They never
// scored deliveries, so switching them on is the user's call (weight > 0).
var SpamCheckCategories = []string{
	"chain", "spf", "rbl", "dkim", "headers", "content",
	"attachments", "links", "embedded", "sender", "emojis",
	"subject_heuristics", "sender_heuristics", "link_heuristics",
}

// DefaultCategoryWeight is the weight of a category the user has not set:
// 1.0 for the stock categories, 0 (off) for the opt-in heuristics.
func DefaultCategoryWeight(name string) float64 {
	switch name {
	case "subject_heuristics", "sender_heuristics", "link_heuristics":
		return 0
	}
	return 1.0
}

// ruleHits collects the rules of one category that fired, in order.
type ruleHits []ruleHit

type ruleHit struct {
	score  float64
	reason string
}

func (h *ruleHits) add(score float64, reason string) {
	*h = append(*h, ruleHit{score: score, reason: reason})
}

// total is the category's raw score — summed in firing order, exactly as the
// category functions accumulated it before findings existed.
func (h ruleHits) total() float64 {
	var s float64
	for _, x := range h {
		s += x.score
	}
	return s
}

// AnalyzeWithUserConfig is the full-power variant: in addition to disabled
// checks (binary off/on), it applies a per-category weight multiplier. A
// weight of 0 means "skip this category entirely" (no score, no reasons),
// 1.0 keeps stock behaviour, anything in between or above scales the
// category's contribution. Missing keys default to DefaultCategoryWeight.
//
// Besides SpamScore/SpamStatus/SpamReasons it fills SpamFindings — the same
// rules with their category and weighted score — so an explanation of the
// verdict is built from this very analysis, not from a second engine.
func (a *Analyzer) AnalyzeWithUserConfig(msg *ParsedMessage, senderIP, fromDomain string, disabledChecks map[string]bool, weights map[string]float64) {
	if !a.config.Enabled {
		msg.SpamStatus = SpamStatusClean
		return
	}

	var totalScore float64
	var reasons []string
	var findings []SpamFinding

	// Fallback: derive sender IP from Received headers when caller didn't pass one.
	// This makes IMAP-synced messages get the same checks as MX-delivered ones.
	if senderIP == "" && msg.RawHeaders != nil {
		hops := ParseReceivedChain(msg.RawHeaders)
		senderIP = ExtractOriginSenderIP(hops)
	}
	// Fallback: derive From-domain from From: header
	if fromDomain == "" && msg.From != nil {
		fromDomain = extractDomain(msg.From.Address)
	}

	// Initialize auth results
	if msg.AuthResults == nil {
		msg.AuthResults = &AuthResults{SenderIP: senderIP}
	} else if msg.AuthResults.SenderIP == "" {
		msg.AuthResults.SenderIP = senderIP
	}

	apply := func(name string, hits ruleHits) {
		if disabledChecks[name] {
			return
		}
		w := categoryWeight(weights, name)
		if w == 0 {
			return
		}
		totalScore += hits.total() * w
		for _, h := range hits {
			reasons = append(reasons, h.reason)
			findings = append(findings, SpamFinding{Check: name, Score: h.score * w, Reason: h.reason})
		}
	}

	// Analyze the Received chain itself (missing headers, time anomalies, suspicious MTA names)
	apply("chain", a.analyzeReceivedChain(msg))

	// Check SPF (if sender IP provided and not disabled)
	if a.spfChecker != nil && senderIP != "" && fromDomain != "" && !IsPrivateIP(senderIP) && !disabledChecks["spf"] && categoryWeight(weights, "spf") > 0 {
		apply("spf", a.analyzeSPF(senderIP, fromDomain, msg))
	}

	// Check RBL (if sender IP provided and not disabled)
	if a.rblChecker != nil && senderIP != "" && !IsPrivateIP(senderIP) && !disabledChecks["rbl"] && categoryWeight(weights, "rbl") > 0 {
		apply("rbl", a.analyzeRBL(senderIP, msg))
	}

	// Check DKIM (if raw message data available and not disabled)
	if a.dkimChecker != nil && len(msg.RawData) > 0 && !disabledChecks["dkim"] && categoryWeight(weights, "dkim") > 0 {
		apply("dkim", a.analyzeDKIM(msg))
	}

	// Check headers
	if a.config.CheckHeaders {
		apply("headers", a.analyzeHeaders(msg))
	}

	// Check content
	if a.config.CheckContent && categoryWeight(weights, "content") > 0 {
		apply("content", a.analyzeContent(msg, disabledChecks))
	}

	// Check attachments
	if a.config.CheckAttachments {
		apply("attachments", a.analyzeAttachments(msg))
	}

	// Check links
	if a.config.CheckLinks && categoryWeight(weights, "links") > 0 {
		apply("links", a.analyzeLinks(msg, disabledChecks))
	}

	// Check embedded messages
	if len(msg.EmbeddedMessages) > 0 {
		apply("embedded", ruleHits{{score: 2.0, reason: "contains embedded message (message/rfc822)"}})
	}

	// Check for brand impersonation and scam sender names
	if msg.From != nil {
		apply("sender", a.analyzeSenderBrand(msg))
	}

	// Check for emojis in subject (common spam indicator)
	apply("emojis", a.analyzeEmojis(msg))

	// Opt-in heuristics (default weight 0 — see DefaultCategoryWeight).
	if categoryWeight(weights, "subject_heuristics") > 0 {
		apply("subject_heuristics", a.analyzeSubjectHeuristics(msg))
	}
	if msg.From != nil && categoryWeight(weights, "sender_heuristics") > 0 {
		apply("sender_heuristics", a.analyzeSenderHeuristics(msg))
	}
	if categoryWeight(weights, "link_heuristics") > 0 {
		apply("link_heuristics", a.analyzeLinkHeuristics(msg))
	}

	// Set final score and status
	msg.SpamScore = totalScore
	msg.SpamReasons = reasons
	msg.SpamFindings = findings
	msg.SpamStatus = a.Verdict(totalScore)
}

// Verdict maps a score to a status with this analyzer's thresholds.
func (a *Analyzer) Verdict(score float64) SpamStatus {
	switch {
	case score >= a.config.SpamThreshold:
		return SpamStatusSpam
	case score >= a.config.SuspiciousThreshold:
		return SpamStatusSuspicious
	default:
		return SpamStatusClean
	}
}

// categoryWeight returns the per-category multiplier. Nil map or missing key
// → DefaultCategoryWeight (stock behaviour). A negative value is clamped to 0
// so the analyzer never accidentally subtracts from the score.
func categoryWeight(weights map[string]float64, name string) float64 {
	w, ok := weights[name]
	if !ok {
		return DefaultCategoryWeight(name)
	}
	if w < 0 {
		return 0
	}
	return w
}

// analyzeSPF performs SPF check
func (a *Analyzer) analyzeSPF(senderIP, fromDomain string, msg *ParsedMessage) ruleHits {
	var hits ruleHits

	result, detail := a.spfChecker.CheckSPF(senderIP, fromDomain)
	msg.AuthResults.SPF = result

	switch result {
	case AuthResultFail:
		hits.add(3.0, "SPF fail: "+detail)
	case AuthResultSoftfail:
		hits.add(1.5, "SPF softfail: "+detail)
	case AuthResultNeutral:
		// No score change for neutral
	case AuthResultPass:
		// Good - could reduce score in future
	}

	return hits
}

// analyzeRBL performs RBL check
func (a *Analyzer) analyzeRBL(senderIP string, msg *ParsedMessage) ruleHits {
	var hits ruleHits

	// CheckIP's score is the sum of the listed results' weights, in order.
	_, results := a.rblChecker.CheckIP(senderIP)
	for _, r := range results {
		hits.add(r.Weight, "RBL listed: "+r.ListName)
	}

	return hits
}

// analyzeDKIM performs DKIM verification
func (a *Analyzer) analyzeDKIM(msg *ParsedMessage) ruleHits {
	var hits ruleHits

	result, detail := a.dkimChecker.CheckDKIM(msg.RawData)
	msg.AuthResults.DKIM = result

	switch result {
	case AuthResultFail:
		hits.add(2.0, "DKIM fail: "+detail)
	case AuthResultPass:
		// Good - could reduce score in future
	}

	return hits
}

// analyzeHeaders checks for suspicious header patterns
func (a *Analyzer) analyzeHeaders(msg *ParsedMessage) ruleHits {
	var hits ruleHits

	// Check if From != Reply-To (potential phishing)
	if msg.From != nil && msg.ReplyTo != nil {
		fromDomain := extractDomain(msg.From.Address)
		replyToDomain := extractDomain(msg.ReplyTo.Address)
		if fromDomain != "" && replyToDomain != "" && fromDomain != replyToDomain {
			hits.add(1.5, "From domain differs from Reply-To domain")
		}
	}

	// Check for missing Message-ID
	if msg.MessageID == "" {
		hits.add(0.5, "missing Message-ID header")
	}

	// Check for missing Date
	if msg.Date.IsZero() {
		hits.add(0.5, "missing Date header")
	}

	// Check for too many Received headers (many hops)
	receivedHeaders := msg.RawHeaders["Received"]
	if len(receivedHeaders) > 10 {
		hits.add(1.0, "excessive mail hops (>10 Received headers)")
	}

	// Check for suspicious X-Mailer or User-Agent
	mailer := ""
	if xmailer, ok := msg.RawHeaders["X-Mailer"]; ok && len(xmailer) > 0 {
		mailer = strings.ToLower(xmailer[0])
	}
	if ua, ok := msg.RawHeaders["User-Agent"]; ok && len(ua) > 0 {
		mailer = strings.ToLower(ua[0])
	}
	if mailer != "" {
		suspiciousMailers := []string{"phpmailer", "swiftmailer", "mass mail"}
		for _, sm := range suspiciousMailers {
			if strings.Contains(mailer, sm) {
				hits.add(0.5, "suspicious mail client: "+sm)
				break
			}
		}
	}

	return hits
}

// analyzeContent checks for spam words and patterns in content
func (a *Analyzer) analyzeContent(msg *ParsedMessage, disabledChecks map[string]bool) ruleHits {
	var hits ruleHits

	// Combine subject, body and HTML body for analysis
	htmlText := ""
	if msg.BodyHTML != "" {
		htmlText = stripHTML(msg.BodyHTML)
	}
	content := strings.ToLower(msg.Subject + " " + msg.Body + " " + htmlText)

	// Check for spam words (can be individually disabled as "spam_word:xxx")
	for _, word := range a.config.SpamWords {
		checkName := "spam_word:" + strings.ToLower(word)
		if disabledChecks[checkName] {
			continue
		}
		if strings.Contains(content, strings.ToLower(word)) {
			hits.add(0.5, "spam word: "+word)
			if hits.total() >= 3.0 {
				// Cap content spam word score
				break
			}
		}
	}

	// Check for excessive caps in subject
	if len(msg.Subject) > 10 {
		upperCount := 0
		for _, r := range msg.Subject {
			if r >= 'A' && r <= 'Z' {
				upperCount++
			}
		}
		if float64(upperCount)/float64(len(msg.Subject)) > 0.5 {
			hits.add(1.0, "excessive caps in subject")
		}
	}

	// Check for HTML-only message (no plain text)
	if msg.BodyHTML != "" && msg.Body == "" {
		hits.add(0.5, "HTML-only message (no plain text)")
	}

	// Check for mostly images in HTML (image-to-text ratio)
	if msg.BodyHTML != "" {
		imgCount := strings.Count(strings.ToLower(msg.BodyHTML), "<img")
		textLen := len(stripHTML(msg.BodyHTML))
		if imgCount > 3 && textLen < 100 {
			hits.add(2.0, "mostly images, little text")
		}
	}

	return hits
}

// analyzeAttachments checks for dangerous attachments
func (a *Analyzer) analyzeAttachments(msg *ParsedMessage) ruleHits {
	var hits ruleHits

	for _, att := range msg.Attachments {
		if att.IsDangerous {
			hits.add(5.0, "dangerous attachment: "+att.Filename)
		}

		// Check for double extensions
		if hasDoubleExtension(att.Filename) {
			hits.add(4.0, "double extension: "+att.Filename)
		}

		// Check for very large attachments (>25MB)
		if att.Size > 25*1024*1024 {
			hits.add(1.0, "large attachment: "+att.Filename)
		}
	}

	return hits
}

// analyzeLinks checks for suspicious URLs
func (a *Analyzer) analyzeLinks(msg *ParsedMessage, disabledChecks map[string]bool) ruleHits {
	var hits ruleHits

	// Extract URLs from body and HTML
	urls := extractURLs(msg.Body + " " + msg.BodyHTML)

	// Count unique domains
	domains := make(map[string]bool)
	shortenerCount := 0

	for _, u := range urls {
		parsed, err := url.Parse(u)
		if err != nil {
			continue
		}
		host := strings.ToLower(parsed.Host)
		domains[host] = true

		// Check for URL shorteners (can be disabled as "url_shortener")
		if !disabledChecks["url_shortener"] {
			for _, shortener := range a.config.URLShorteners {
				if strings.Contains(host, shortener) {
					shortenerCount++
					break
				}
			}
		}

		// Check for suspicious patterns (typosquatting)
		if isSuspiciousDomain(host) {
			hits.add(3.0, "suspicious domain: "+host)
		}
	}

	// Too many links
	if len(urls) > 10 {
		hits.add(1.0, "excessive links (>10)")
	}

	// URL shorteners
	if shortenerCount > 0 {
		hits.add(float64(shortenerCount)*0.5, "contains URL shortener(s)")
	}

	return hits
}

// GetSpamReasonsJSON returns spam reasons as JSON string
func GetSpamReasonsJSON(reasons []string) string {
	if len(reasons) == 0 {
		return "[]"
	}
	data, err := json.Marshal(reasons)
	if err != nil {
		return "[]"
	}
	return string(data)
}

// Helper functions

func extractDomain(email string) string {
	parts := strings.Split(email, "@")
	if len(parts) == 2 {
		return strings.ToLower(parts[1])
	}
	return ""
}

func hasDoubleExtension(filename string) bool {
	// Check for patterns like file.pdf.exe
	parts := strings.Split(filename, ".")
	if len(parts) < 3 {
		return false
	}

	// Check if last extension is dangerous
	lastExt := "." + strings.ToLower(parts[len(parts)-1])
	for _, dangerous := range DangerousExtensions {
		if lastExt == dangerous {
			return true
		}
	}
	return false
}

var urlRegex = regexp.MustCompile(`https?://[^\s<>"']+`)

func extractURLs(text string) []string {
	return urlRegex.FindAllString(text, -1)
}

func isSuspiciousDomain(domain string) bool {
	// Check for typosquatting patterns
	suspiciousPatterns := []struct {
		legit string
		typos []string
	}{
		{"google.com", []string{"g00gle", "googel", "gooogle", "goog1e"}},
		{"microsoft.com", []string{"micros0ft", "mircosoft", "microsft"}},
		{"apple.com", []string{"app1e", "applle", "aple"}},
		{"amazon.com", []string{"amaz0n", "amazn", "arnazon"}},
		{"paypal.com", []string{"paypa1", "paypai", "paypaI"}},
		{"facebook.com", []string{"faceb00k", "facebok", "faceboook"}},
	}

	for _, p := range suspiciousPatterns {
		for _, typo := range p.typos {
			if strings.Contains(domain, typo) {
				return true
			}
		}
	}

	return false
}

// Known brands with their legitimate domains
var knownBrands = map[string][]string{
	// Russian retail
	"dns":         {"dns-shop.ru", "dns.ru"},
	"ozon":        {"ozon.ru"},
	"wildberries": {"wildberries.ru", "wb.ru"},
	"mvideo":      {"mvideo.ru", "m-video.ru"},
	"мвидео":      {"mvideo.ru", "m-video.ru"},
	"м.видео":     {"mvideo.ru", "m-video.ru"},
	"eldorado":    {"eldorado.ru"},
	"эльдорадо":   {"eldorado.ru"},
	"citilink":    {"citilink.ru"},
	"ситилинк":    {"citilink.ru"},
	"lamoda":      {"lamoda.ru"},
	"ламода":      {"lamoda.ru"},
	"aliexpress":  {"aliexpress.ru", "aliexpress.com"},
	// Russian banks
	"sberbank":   {"sberbank.ru", "sber.ru"},
	"сбербанк":   {"sberbank.ru", "sber.ru"},
	"сбер":       {"sberbank.ru", "sber.ru"},
	"tinkoff":    {"tinkoff.ru", "tbank.ru"},
	"тинькофф":   {"tinkoff.ru", "tbank.ru"},
	"т-банк":     {"tinkoff.ru", "tbank.ru"},
	"vtb":        {"vtb.ru"},
	"втб":        {"vtb.ru"},
	"alfa":       {"alfabank.ru", "alfa.ru"},
	"альфа":      {"alfabank.ru", "alfa.ru"},
	"альфа-банк": {"alfabank.ru", "alfa.ru"},
	"gazprom":    {"gazprombank.ru", "gazprom.ru"},
	"газпром":    {"gazprombank.ru", "gazprom.ru"},
	// International
	"paypal":     {"paypal.com", "paypal.ru"},
	"amazon":     {"amazon.com", "amazon.ru"},
	"apple":      {"apple.com", "apple.ru"},
	"microsoft":  {"microsoft.com", "microsoft.ru", "outlook.com"},
	"google":     {"google.com", "google.ru", "gmail.com"},
	"facebook":   {"facebook.com", "fb.com"},
	"meta":       {"meta.com", "facebook.com"},
	"instagram":  {"instagram.com"},
	"netflix":    {"netflix.com"},
	"whatsapp":   {"whatsapp.com"},
	"telegram":   {"telegram.org", "t.me"},
	"visa":       {"visa.com", "visa.ru"},
	"mastercard": {"mastercard.com", "mastercard.ru"},
	"dhl":        {"dhl.com", "dhl.ru"},
	"fedex":      {"fedex.com"},
	"ups":        {"ups.com"},
}

// analyzeSenderBrand checks for brand impersonation and scam sender names
func (a *Analyzer) analyzeSenderBrand(msg *ParsedMessage) ruleHits {
	var hits ruleHits

	if msg.From == nil {
		return nil
	}

	fromName := strings.ToLower(msg.From.Name)
	fromDomain := strings.ToLower(extractDomain(msg.From.Address))

	// Check for brand impersonation
	for brand, legitDomains := range knownBrands {
		if strings.Contains(fromName, brand) {
			// Display name contains a known brand - check if domain is legitimate
			isLegit := false
			for _, legitDomain := range legitDomains {
				if fromDomain == legitDomain || strings.HasSuffix(fromDomain, "."+legitDomain) {
					isLegit = true
					break
				}
			}
			if !isLegit {
				hits.add(5.0, "brand impersonation: \""+msg.From.Name+"\" from "+fromDomain)
				break
			}
		}
	}

	// Check for scam-like sender names (e.g., "Лаборатория дохода")
	scamNamePatterns := []string{
		"лаборатория", "академия", "институт", "центр", "школа", "клуб",
		"система", "платформа", "проект", "команда", "сообщество",
	}
	scamNameKeywords := []string{
		"доход", "заработ", "прибыл", "инвест", "капитал", "финанс",
		"крипт", "трейд", "торгов", "бизнес", "успех", "богат",
	}
	for _, pattern := range scamNamePatterns {
		if strings.Contains(fromName, pattern) {
			for _, keyword := range scamNameKeywords {
				if strings.Contains(fromName, keyword) {
					hits.add(3.0, "scam-like sender: \""+msg.From.Name+"\"")
					break
				}
			}
			break
		}
	}

	return hits
}

// analyzeEmojis checks for emojis in subject (common spam indicator)
func (a *Analyzer) analyzeEmojis(msg *ParsedMessage) ruleHits {
	var hits ruleHits

	emojiCount := 0
	for _, r := range msg.Subject {
		if (r >= 0x1F300 && r <= 0x1F9FF) || // Misc Symbols, Emoticons
			(r >= 0x2600 && r <= 0x26FF) || // Misc symbols
			(r >= 0x2700 && r <= 0x27BF) || // Dingbats
			(r >= 0x1F600 && r <= 0x1F64F) || // Emoticons
			(r >= 0x1F680 && r <= 0x1F6FF) || // Transport
			(r >= 0x1F1E0 && r <= 0x1F1FF) { // Flags
			emojiCount++
		}
	}

	if emojiCount > 0 {
		score := float64(emojiCount) * 0.5
		if score > 2.0 {
			score = 2.0
		}
		hits.add(score, "emojis in subject")
	}

	return hits
}

// analyzeReceivedChain scrutinizes the Received header chain for red flags.
// Self-verifying — does not trust any provider's Authentication-Results.
func (a *Analyzer) analyzeReceivedChain(msg *ParsedMessage) ruleHits {
	var hits ruleHits

	if msg.RawHeaders == nil {
		return nil
	}

	hops := ParseReceivedChain(msg.RawHeaders)

	// 1. No Received headers at all — extremely suspicious for any real email
	if len(hops) == 0 {
		return ruleHits{{score: 4.0, reason: "no Received headers"}}
	}

	// 2. Single Received hop — usually means message was injected directly without traversing the network
	if len(hops) == 1 {
		hits.add(1.5, "only one Received hop")
	}

	// 3. Time progression: hops should be ordered newest→oldest, dates monotonically decreasing
	var lastDate time.Time
	for i, h := range hops {
		if h.Date.IsZero() {
			continue
		}
		if i > 0 && !lastDate.IsZero() {
			// h is older than lastDate (or equal). It must NOT be after lastDate by more than a few minutes.
			if h.Date.After(lastDate.Add(5 * time.Minute)) {
				hits.add(1.5, "Received chain timestamps inconsistent")
				break
			}
		}
		lastDate = h.Date
	}

	// 4. Origin hop — examine the deepest/oldest non-private hop
	origin := ExtractOriginHop(hops)
	if origin != nil {
		// HELO claims a name that looks nothing like its PTR
		if origin.From != "" && origin.FromPTR != "" {
			if !heloMatchesPTR(origin.From, origin.FromPTR) {
				hits.add(1.0, "HELO doesn't match reverse DNS")
			}
		}
		// PTR is "unknown" or absent on a public IP
		if origin.FromPTR == "" || strings.EqualFold(origin.FromPTR, "unknown") {
			hits.add(0.5, "no reverse DNS for sending IP")
		}
		// Suspicious MTA name (auth-XXXX-N.foo.bar pattern, random subdomains)
		if isSuspiciousMTAName(origin.From) {
			hits.add(2.0, "suspicious sending MTA name: "+origin.From)
		}
	}

	return hits
}

// heloMatchesPTR returns true if the HELO hostname and PTR hostname share
// at least the registrable second-level domain.
func heloMatchesPTR(helo, ptr string) bool {
	helo = strings.ToLower(strings.TrimRight(helo, "."))
	ptr = strings.ToLower(strings.TrimRight(ptr, "."))
	if helo == ptr {
		return true
	}
	heloParts := strings.Split(helo, ".")
	ptrParts := strings.Split(ptr, ".")
	if len(heloParts) < 2 || len(ptrParts) < 2 {
		return false
	}
	heloRoot := heloParts[len(heloParts)-2] + "." + heloParts[len(heloParts)-1]
	ptrRoot := ptrParts[len(ptrParts)-2] + "." + ptrParts[len(ptrParts)-1]
	return heloRoot == ptrRoot
}

// isSuspiciousMTAName detects patterns typical of spam farms:
// random alphanumeric subdomains like "auth-jxzq-7.vcp.example.com",
// "mta-xkfg-3.relay.example.org", etc.
func isSuspiciousMTAName(name string) bool {
	name = strings.ToLower(name)
	if name == "" {
		return false
	}
	parts := strings.Split(name, ".")
	if len(parts) < 2 {
		return false
	}
	first := parts[0]
	if !strings.Contains(first, "-") {
		return false
	}
	chunks := strings.Split(first, "-")
	if len(chunks) < 3 {
		return false
	}
	// Look for a chunk that looks random: 3-6 chars, mostly consonants or digits
	for _, c := range chunks {
		if len(c) >= 3 && len(c) <= 6 && looksRandom(c) {
			return true
		}
	}
	return false
}

// looksRandom returns true for short strings with very few or very many vowels,
// which is typical of generated identifiers rather than dictionary words.
func looksRandom(s string) bool {
	vowels := 0
	letters := 0
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			letters++
			if r == 'a' || r == 'e' || r == 'i' || r == 'o' || r == 'u' || r == 'y' {
				vowels++
			}
		}
	}
	if letters == 0 {
		return false
	}
	ratio := float64(vowels) / float64(letters)
	// Real words tend to have 30-60% vowels. Random strings deviate.
	return ratio < 0.15 || ratio > 0.75
}
