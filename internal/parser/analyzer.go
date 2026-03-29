package parser

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
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
	if !a.config.Enabled {
		msg.SpamStatus = SpamStatusClean
		return
	}

	var totalScore float64
	var reasons []string

	// Initialize auth results
	if msg.AuthResults == nil {
		msg.AuthResults = &AuthResults{SenderIP: senderIP}
	}

	// Check SPF (if sender IP provided and not disabled)
	if a.spfChecker != nil && senderIP != "" && fromDomain != "" && !IsPrivateIP(senderIP) && !disabledChecks["spf"] {
		score, spfReasons := a.analyzeSPF(senderIP, fromDomain, msg)
		totalScore += score
		reasons = append(reasons, spfReasons...)
	}

	// Check RBL (if sender IP provided and not disabled)
	if a.rblChecker != nil && senderIP != "" && !IsPrivateIP(senderIP) && !disabledChecks["rbl"] {
		score, rblReasons := a.analyzeRBL(senderIP, msg)
		totalScore += score
		reasons = append(reasons, rblReasons...)
	}

	// Check DKIM (if raw message data available and not disabled)
	if a.dkimChecker != nil && len(msg.RawData) > 0 && !disabledChecks["dkim"] {
		score, dkimReasons := a.analyzeDKIM(msg)
		totalScore += score
		reasons = append(reasons, dkimReasons...)
	}

	// Check headers
	if a.config.CheckHeaders {
		score, headerReasons := a.analyzeHeaders(msg)
		totalScore += score
		reasons = append(reasons, headerReasons...)
	}

	// Check content
	if a.config.CheckContent {
		score, contentReasons := a.analyzeContent(msg, disabledChecks)
		totalScore += score
		reasons = append(reasons, contentReasons...)
	}

	// Check attachments
	if a.config.CheckAttachments {
		score, attachReasons := a.analyzeAttachments(msg)
		totalScore += score
		reasons = append(reasons, attachReasons...)
	}

	// Check links
	if a.config.CheckLinks {
		score, linkReasons := a.analyzeLinks(msg, disabledChecks)
		totalScore += score
		reasons = append(reasons, linkReasons...)
	}

	// Check embedded messages
	if len(msg.EmbeddedMessages) > 0 {
		totalScore += 2.0
		reasons = append(reasons, "contains embedded message (message/rfc822)")
	}

	// Check for brand impersonation and scam sender names
	if msg.From != nil {
		score, senderReasons := a.analyzeSenderBrand(msg)
		totalScore += score
		reasons = append(reasons, senderReasons...)
	}

	// Check for emojis in subject (common spam indicator)
	emojiScore, emojiReasons := a.analyzeEmojis(msg)
	totalScore += emojiScore
	reasons = append(reasons, emojiReasons...)

	// Set final score and status
	msg.SpamScore = totalScore
	msg.SpamReasons = reasons

	if totalScore >= a.config.SpamThreshold {
		msg.SpamStatus = SpamStatusSpam
	} else if totalScore >= a.config.SuspiciousThreshold {
		msg.SpamStatus = SpamStatusSuspicious
	} else {
		msg.SpamStatus = SpamStatusClean
	}
}

// analyzeSPF performs SPF check
func (a *Analyzer) analyzeSPF(senderIP, fromDomain string, msg *ParsedMessage) (float64, []string) {
	var score float64
	var reasons []string

	result, detail := a.spfChecker.CheckSPF(senderIP, fromDomain)
	msg.AuthResults.SPF = result

	switch result {
	case AuthResultFail:
		score += 3.0
		reasons = append(reasons, "SPF fail: "+detail)
	case AuthResultSoftfail:
		score += 1.5
		reasons = append(reasons, "SPF softfail: "+detail)
	case AuthResultNeutral:
		// No score change for neutral
	case AuthResultPass:
		// Good - could reduce score in future
	}

	return score, reasons
}

// analyzeRBL performs RBL check
func (a *Analyzer) analyzeRBL(senderIP string, msg *ParsedMessage) (float64, []string) {
	var reasons []string

	score, results := a.rblChecker.CheckIP(senderIP)

	if len(results) > 0 {
		for _, r := range results {
			reasons = append(reasons, "RBL listed: "+r.ListName)
		}
	}

	return score, reasons
}

// analyzeDKIM performs DKIM verification
func (a *Analyzer) analyzeDKIM(msg *ParsedMessage) (float64, []string) {
	var score float64
	var reasons []string

	result, detail := a.dkimChecker.CheckDKIM(msg.RawData)
	msg.AuthResults.DKIM = result

	switch result {
	case AuthResultFail:
		score += 2.0
		reasons = append(reasons, "DKIM fail: "+detail)
	case AuthResultPass:
		// Good - could reduce score in future
	}

	return score, reasons
}

// analyzeHeaders checks for suspicious header patterns
func (a *Analyzer) analyzeHeaders(msg *ParsedMessage) (float64, []string) {
	var score float64
	var reasons []string

	// Check if From != Reply-To (potential phishing)
	if msg.From != nil && msg.ReplyTo != nil {
		fromDomain := extractDomain(msg.From.Address)
		replyToDomain := extractDomain(msg.ReplyTo.Address)
		if fromDomain != "" && replyToDomain != "" && fromDomain != replyToDomain {
			score += 1.5
			reasons = append(reasons, "From domain differs from Reply-To domain")
		}
	}

	// Check for missing Message-ID
	if msg.MessageID == "" {
		score += 0.5
		reasons = append(reasons, "missing Message-ID header")
	}

	// Check for missing Date
	if msg.Date.IsZero() {
		score += 0.5
		reasons = append(reasons, "missing Date header")
	}

	// Check for too many Received headers (many hops)
	receivedHeaders := msg.RawHeaders["Received"]
	if len(receivedHeaders) > 10 {
		score += 1.0
		reasons = append(reasons, "excessive mail hops (>10 Received headers)")
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
				score += 0.5
				reasons = append(reasons, "suspicious mail client: "+sm)
				break
			}
		}
	}

	return score, reasons
}

// analyzeContent checks for spam words and patterns in content
func (a *Analyzer) analyzeContent(msg *ParsedMessage, disabledChecks map[string]bool) (float64, []string) {
	var score float64
	var reasons []string

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
			score += 0.5
			reasons = append(reasons, "spam word: "+word)
			if score >= 3.0 {
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
			score += 1.0
			reasons = append(reasons, "excessive caps in subject")
		}
	}

	// Check for HTML-only message (no plain text)
	if msg.BodyHTML != "" && msg.Body == "" {
		score += 0.5
		reasons = append(reasons, "HTML-only message (no plain text)")
	}

	// Check for mostly images in HTML (image-to-text ratio)
	if msg.BodyHTML != "" {
		imgCount := strings.Count(strings.ToLower(msg.BodyHTML), "<img")
		textLen := len(stripHTML(msg.BodyHTML))
		if imgCount > 3 && textLen < 100 {
			score += 2.0
			reasons = append(reasons, "mostly images, little text")
		}
	}

	return score, reasons
}

// analyzeAttachments checks for dangerous attachments
func (a *Analyzer) analyzeAttachments(msg *ParsedMessage) (float64, []string) {
	var score float64
	var reasons []string

	for _, att := range msg.Attachments {
		if att.IsDangerous {
			score += 5.0
			reasons = append(reasons, "dangerous attachment: "+att.Filename)
		}

		// Check for double extensions
		if hasDoubleExtension(att.Filename) {
			score += 4.0
			reasons = append(reasons, "double extension: "+att.Filename)
		}

		// Check for very large attachments (>25MB)
		if att.Size > 25*1024*1024 {
			score += 1.0
			reasons = append(reasons, "large attachment: "+att.Filename)
		}
	}

	return score, reasons
}

// analyzeLinks checks for suspicious URLs
func (a *Analyzer) analyzeLinks(msg *ParsedMessage, disabledChecks map[string]bool) (float64, []string) {
	var score float64
	var reasons []string

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
			score += 3.0
			reasons = append(reasons, "suspicious domain: "+host)
		}
	}

	// Too many links
	if len(urls) > 10 {
		score += 1.0
		reasons = append(reasons, "excessive links (>10)")
	}

	// URL shorteners
	if shortenerCount > 0 {
		score += float64(shortenerCount) * 0.5
		reasons = append(reasons, "contains URL shortener(s)")
	}

	return score, reasons
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
func (a *Analyzer) analyzeSenderBrand(msg *ParsedMessage) (float64, []string) {
	var score float64
	var reasons []string

	if msg.From == nil {
		return 0, nil
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
				score += 5.0
				reasons = append(reasons, "brand impersonation: \""+msg.From.Name+"\" from "+fromDomain)
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
					score += 3.0
					reasons = append(reasons, "scam-like sender: \""+msg.From.Name+"\"")
					break
				}
			}
			break
		}
	}

	return score, reasons
}

// analyzeEmojis checks for emojis in subject (common spam indicator)
func (a *Analyzer) analyzeEmojis(msg *ParsedMessage) (float64, []string) {
	var score float64
	var reasons []string

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
		score = float64(emojiCount) * 0.5
		if score > 2.0 {
			score = 2.0
		}
		reasons = append(reasons, "emojis in subject")
	}

	return score, reasons
}
