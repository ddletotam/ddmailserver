package web

import (
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gorilla/mux"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// SpamData holds data for the spam page
type SpamData struct {
	PageData
	Messages     []*models.Message
	MessageCount int
	TotalCount   int
	Page         int
	PageSize     int
	PrevPage     int
	NextPage     int
	HasNextPage  bool
}

// SpamRulesData holds data for the spam rules page
type SpamRulesData struct {
	PageData
	Rules []*db.SpamRule
}

// SpamSettingsData holds data for the spam settings page
type SpamSettingsData struct {
	PageData
	DisabledChecks  []string
	AvailableChecks []SpamCheck
}

// SpamCheck represents a spam check that can be enabled/disabled
type SpamCheck struct {
	Name        string
	Description string
	Enabled     bool
}

// SpamAnalysisData holds analysis data for a spam message
type SpamAnalysisData struct {
	SpamScore      float64         `json:"spam_score"`
	SpamStatus     string          `json:"spam_status"`
	SpamReasons    []string        `json:"spam_reasons"`
	SuggestedRules []SuggestedRule `json:"suggested_rules"`
}

// SuggestedRule is a rule suggestion from spam analysis
type SuggestedRule struct {
	Type   string `json:"type"`   // "address" or "domain"
	Value  string `json:"value"`  // email or domain
	Action string `json:"action"` // "spam" or "allow"
}

// HandleSpamPage displays spam messages
func (s *Server) HandleSpamPage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	// Pagination
	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil && parsed > 0 {
			page = parsed
		}
	}
	pageSize := 50
	offset := (page - 1) * pageSize

	// Get spam messages
	messages, total, err := s.database.GetSpamMessages(user.ID, pageSize, offset)
	if err != nil {
		log.Printf("Failed to get spam messages: %v", err)
		messages = nil
		total = 0
	}

	// Get user's language for title translation
	userLang := user.Language
	if userLang == "" {
		userLang = "en"
	}
	i18n := s.i18nManager.Get(userLang)

	data := SpamData{
		PageData: PageData{
			Title: i18n.T("spam.title"),
			User:  user,
		},
		Messages:     messages,
		MessageCount: len(messages),
		TotalCount:   total,
		Page:         page,
		PageSize:     pageSize,
		PrevPage:     page - 1,
		NextPage:     page + 1,
		HasNextPage:  page*pageSize < total,
	}

	s.renderTemplate(w, "spam.html", data)
}

// HandleRestoreFromSpam restores a message from spam to inbox
func (s *Server) HandleRestoreFromSpam(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	messageID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid message ID", http.StatusBadRequest)
		return
	}

	if _, err := s.database.GetMessageByIDForUser(messageID, user.ID); err != nil {
		http.Error(w, "Message not found", http.StatusNotFound)
		return
	}

	// Restore from spam
	if err := s.database.RestoreFromSpam(messageID, user.ID); err != nil {
		log.Printf("Failed to restore from spam: %v", err)
		http.Error(w, "Failed to restore", http.StatusInternalServerError)
		return
	}

	// For htmx, return empty response to remove the row
	w.Header().Set("HX-Trigger", "spamRestored")
	w.WriteHeader(http.StatusOK)
}

// HandleDeleteSpamMessage permanently deletes a spam message
func (s *Server) HandleDeleteSpamMessage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	messageID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid message ID", http.StatusBadRequest)
		return
	}

	if _, err := s.database.GetMessageByIDForUser(messageID, user.ID); err != nil {
		http.Error(w, "Message not found", http.StatusNotFound)
		return
	}

	// Hard delete message
	if err := s.database.HardDeleteMessage(messageID); err != nil {
		log.Printf("Failed to permanently delete spam: %v", err)
		http.Error(w, "Failed to delete", http.StatusInternalServerError)
		return
	}

	// Remove from search index if available
	if s.searchIndexer != nil {
		s.searchIndexer.DeleteMessage(messageID)
	}

	w.Header().Set("HX-Trigger", "spamDeleted")
	w.WriteHeader(http.StatusOK)
}

// HandleSpamRulesPage displays user's spam rules
func (s *Server) HandleSpamRulesPage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	rules, err := s.database.GetSpamRulesByUserID(user.ID)
	if err != nil {
		log.Printf("Failed to get spam rules: %v", err)
		rules = nil
	}

	// Get user's language for title translation
	userLang := user.Language
	if userLang == "" {
		userLang = "en"
	}
	i18n := s.i18nManager.Get(userLang)

	data := SpamRulesData{
		PageData: PageData{
			Title: i18n.T("spam.rules.title"),
			User:  user,
		},
		Rules: rules,
	}

	s.renderTemplate(w, "spam_rules.html", data)
}

// HandleCreateSpamRule creates a new spam rule
func (s *Server) HandleCreateSpamRule(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	ruleType := r.FormValue("rule_type")   // "address" or "domain"
	ruleValue := r.FormValue("rule_value") // email or domain
	action := r.FormValue("action")        // "spam" or "allow"

	// Validate
	if ruleType != "address" && ruleType != "domain" {
		http.Error(w, "Invalid rule type", http.StatusBadRequest)
		return
	}
	if action != "spam" && action != "allow" {
		http.Error(w, "Invalid action", http.StatusBadRequest)
		return
	}
	if ruleValue == "" {
		http.Error(w, "Value required", http.StatusBadRequest)
		return
	}

	// Create rule
	rule := &db.SpamRule{
		UserID:    user.ID,
		RuleType:  ruleType,
		RuleValue: strings.ToLower(ruleValue),
		Action:    action,
	}

	if err := s.database.CreateSpamRule(rule); err != nil {
		log.Printf("Failed to create spam rule: %v", err)
		http.Error(w, "Failed to create rule", http.StatusInternalServerError)
		return
	}

	// Redirect back to rules page
	http.Redirect(w, r, "/spam/rules", http.StatusSeeOther)
}

// HandleDeleteSpamRule deletes a spam rule
func (s *Server) HandleDeleteSpamRule(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	ruleID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid rule ID", http.StatusBadRequest)
		return
	}

	// Verify rule belongs to user
	rule, err := s.database.GetSpamRuleByID(ruleID)
	if err != nil || rule == nil || rule.UserID != user.ID {
		http.Error(w, "Rule not found", http.StatusNotFound)
		return
	}

	// Delete rule
	if err := s.database.DeleteSpamRule(ruleID); err != nil {
		log.Printf("Failed to delete spam rule: %v", err)
		http.Error(w, "Failed to delete rule", http.StatusInternalServerError)
		return
	}

	w.Header().Set("HX-Trigger", "ruleDeleted")
	w.WriteHeader(http.StatusOK)
}

// HandleSpamSettingsPage displays spam settings (disabled checks)
func (s *Server) HandleSpamSettingsPage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	disabledChecks, err := s.database.GetDisabledSpamChecks(user.ID)
	if err != nil {
		log.Printf("Failed to get disabled spam checks: %v", err)
		disabledChecks = nil
	}

	// Build map for quick lookup
	disabledMap := make(map[string]bool)
	for _, check := range disabledChecks {
		disabledMap[check] = true
	}

	// Get user's language for title translation
	userLang := user.Language
	if userLang == "" {
		userLang = "en"
	}
	i18n := s.i18nManager.Get(userLang)

	// Available checks
	availableChecks := []SpamCheck{
		{Name: "spf", Description: i18n.T("spam.check.spf"), Enabled: !disabledMap["spf"]},
		{Name: "dkim", Description: i18n.T("spam.check.dkim"), Enabled: !disabledMap["dkim"]},
		{Name: "rbl", Description: i18n.T("spam.check.rbl"), Enabled: !disabledMap["rbl"]},
		{Name: "url_shortener", Description: i18n.T("spam.check.url_shortener"), Enabled: !disabledMap["url_shortener"]},
	}

	data := SpamSettingsData{
		PageData: PageData{
			Title: i18n.T("spam.settings.title"),
			User:  user,
		},
		DisabledChecks:  disabledChecks,
		AvailableChecks: availableChecks,
	}

	s.renderTemplate(w, "spam_settings.html", data)
}

// HandleToggleSpamCheck enables or disables a spam check
func (s *Server) HandleToggleSpamCheck(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	checkName := vars["name"]

	// Validate check name
	validChecks := map[string]bool{
		"spf": true, "dkim": true, "rbl": true, "url_shortener": true,
	}
	if !validChecks[checkName] {
		http.Error(w, "Invalid check name", http.StatusBadRequest)
		return
	}

	// Check current state
	isDisabled, err := s.database.IsSpamCheckDisabled(user.ID, checkName)
	if err != nil {
		log.Printf("Failed to check spam setting: %v", err)
		http.Error(w, "Failed to toggle", http.StatusInternalServerError)
		return
	}

	// Toggle state
	if isDisabled {
		// Enable check
		if err := s.database.EnableSpamCheck(user.ID, checkName); err != nil {
			log.Printf("Failed to enable spam check: %v", err)
			http.Error(w, "Failed to enable", http.StatusInternalServerError)
			return
		}
	} else {
		// Disable check
		if err := s.database.DisableSpamCheck(user.ID, checkName); err != nil {
			log.Printf("Failed to disable spam check: %v", err)
			http.Error(w, "Failed to disable", http.StatusInternalServerError)
			return
		}
	}

	// Return new state for htmx
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"enabled": isDisabled}) // flipped
}

// HandleAnalyzeSpam analyzes a spam message and suggests rules
func (s *Server) HandleAnalyzeSpam(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	messageID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid message ID", http.StatusBadRequest)
		return
	}

	msg, err := s.database.GetMessageByIDForUser(messageID, user.ID)
	if err != nil {
		http.Error(w, "Message not found", http.StatusNotFound)
		return
	}

	// Parse spam reasons from JSON
	var spamReasons []string
	if msg.SpamReasons != "" {
		json.Unmarshal([]byte(msg.SpamReasons), &spamReasons)
	}

	// Generate suggestions based on sender
	var suggestions []SuggestedRule

	// Extract sender address and domain
	fromEmail := extractEmailAddress(msg.From)
	if fromEmail != "" {
		// Suggest blocking by address
		suggestions = append(suggestions, SuggestedRule{
			Type:   "address",
			Value:  fromEmail,
			Action: "spam",
		})

		// Suggest blocking by domain
		parts := strings.SplitN(fromEmail, "@", 2)
		if len(parts) == 2 {
			suggestions = append(suggestions, SuggestedRule{
				Type:   "domain",
				Value:  parts[1],
				Action: "spam",
			})
		}
	}

	data := SpamAnalysisData{
		SpamScore:      msg.SpamScore,
		SpamStatus:     msg.SpamStatus,
		SpamReasons:    spamReasons,
		SuggestedRules: suggestions,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

// HandleMarkAsSpam marks a message as spam
func (s *Server) HandleMarkAsSpam(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	messageID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid message ID", http.StatusBadRequest)
		return
	}

	if _, err := s.database.GetMessageByIDForUser(messageID, user.ID); err != nil {
		http.Error(w, "Message not found", http.StatusNotFound)
		return
	}

	// Mark as spam
	if err := s.database.MarkMessageAsSpam(messageID, nil); err != nil {
		log.Printf("Failed to mark as spam: %v", err)
		http.Error(w, "Failed to mark as spam", http.StatusInternalServerError)
		return
	}

	w.Header().Set("HX-Trigger", "markedAsSpam")
	w.WriteHeader(http.StatusOK)
}

// HandleMarkAsSpamByMessageID marks a message as spam by RFC 5322 Message-ID header
func (s *Server) HandleMarkAsSpamByMessageID(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	messageID := strings.TrimSpace(r.FormValue("message_id"))
	if messageID == "" {
		w.Write([]byte(`<div class="alert alert-danger">Message-ID is required</div>`))
		return
	}

	createRule := r.FormValue("create_rule") == "1"

	// Get message by Message-ID header
	msg, err := s.database.GetMessageByMessageID(user.ID, messageID)
	if err != nil {
		log.Printf("Failed to get message by Message-ID: %v", err)
		w.Write([]byte(`<div class="alert alert-danger">Error searching for message</div>`))
		return
	}
	if msg == nil {
		w.Write([]byte(`<div class="alert alert-warning">Message not found with this Message-ID</div>`))
		return
	}

	// Mark as spam (this removes it from folder queries due to is_spam filter)
	if err := s.database.MarkMessageAsSpam(msg.ID, nil); err != nil {
		log.Printf("Failed to mark as spam: %v", err)
		w.Write([]byte(`<div class="alert alert-danger">Failed to mark as spam</div>`))
		return
	}

	// Extract sender info
	fromEmail := extractEmailAddress(msg.From)

	// Create rule if requested
	if createRule && fromEmail != "" {
		rule := &db.SpamRule{
			UserID:    user.ID,
			RuleType:  "address",
			RuleValue: fromEmail,
			Action:    "spam",
		}
		if err := s.database.CreateSpamRule(rule); err != nil {
			log.Printf("Failed to create spam rule: %v", err)
		}
	}

	// Redirect to reload the page (clears form, refreshes rules)
	w.Header().Set("HX-Redirect", "/spam/rules")
	w.WriteHeader(http.StatusOK)
}

// SpamAnalysisResult holds the results of enhanced spam analysis
type SpamAnalysisResult struct {
	// Basic info
	Subject   string
	From      string
	FromEmail string
	FromName  string
	Domain    string
	Date      string

	// Scores
	TotalScore   float64
	ContentScore float64
	SenderScore  float64
	LinkScore    float64
	HistoryScore float64

	// Issues found
	Issues []SpamIssue

	// Sender stats
	SenderStats *db.SenderStats
	DomainTotal int
	DomainSpam  int

	// Links found
	Links          []string
	ShortenerURLs  []string
	SuspiciousURLs []string

	// Flags
	HasWhitelist        bool
	HasBlacklist        bool
	IsFreeEmailProvider bool
	NameEmailMismatch   bool
	ReplyToMismatch     bool
}

// SpamIssue represents a single spam indicator
type SpamIssue struct {
	Category string // "content", "sender", "link", "history", "header"
	Severity string // "high", "medium", "low"
	Score    float64
	Message  string
}

// Free email providers commonly used for spam
var freeEmailProviders = map[string]bool{
	"gmail.com": true, "yahoo.com": true, "hotmail.com": true, "outlook.com": true,
	"mail.ru": true, "yandex.ru": true, "rambler.ru": true, "bk.ru": true,
	"inbox.ru": true, "list.ru": true, "aol.com": true, "protonmail.com": true,
	"icloud.com": true, "me.com": true, "live.com": true, "msn.com": true,
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

// Suspicious name patterns (brand names that shouldn't come from free email)
var suspiciousBrandNames = []string{
	"paypal", "amazon", "apple", "microsoft", "google", "facebook", "instagram",
	"netflix", "bank", "visa", "mastercard", "support", "security", "admin",
	"service", "account", "verify", "update", "confirm", "sberbank", "tinkoff",
	"vtb", "alfa-bank", "gazprom",
}

// Russian spam trigger words (commercial/promotional)
var russianSpamWords = []string{
	// Sales/marketing
	"распродажа", "купон", "скидка", "акция", "бесплатн", "выигр", "подарок",
	"персональн", "эксклюзив", "только для вас", "только сегодня", "последний шанс",
	"активир", "получи", "забери", "успей", "торопись", "не упусти",
	// Financial scams
	"заработ", "доход", "инвести", "прибыль", "без вложений", "пассивн",
	"кредит", "займ", "одобрен", "микрозайм", "долг", "избавиться от",
	"финансов", "капитал", "бонус на баланс", "приветственный бонус",
	"приглашение в клуб", "закрытый клуб", "vip", "элитн",
	// Crypto/trading scams
	"криптовалют", "биткоин", "трейдинг", "торговл", "сигнал",
	"стратеги", "робот", "автоматическ",
}

// HandleAnalyzeByMessageID analyzes a message for spam by Message-ID
func (s *Server) HandleAnalyzeByMessageID(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	messageID := strings.TrimSpace(r.FormValue("message_id"))
	if messageID == "" {
		w.Write([]byte(`<div class="alert alert-danger">Message-ID is required</div>`))
		return
	}

	// Get message by Message-ID header
	msg, err := s.database.GetMessageByMessageID(user.ID, messageID)
	if err != nil {
		log.Printf("Failed to get message by Message-ID: %v", err)
		w.Write([]byte(`<div class="alert alert-danger">Error searching for message</div>`))
		return
	}
	if msg == nil {
		w.Write([]byte(`<div class="alert alert-warning">Message not found with this Message-ID</div>`))
		return
	}

	// Perform enhanced analysis
	result := s.analyzeMessageForSpam(user.ID, msg)

	// Render results
	s.renderSpamAnalysisResult(w, result, messageID)
}

// analyzeMessageForSpam performs comprehensive spam analysis
func (s *Server) analyzeMessageForSpam(userID int64, msg *models.Message) *SpamAnalysisResult {
	result := &SpamAnalysisResult{
		Subject: msg.Subject,
		From:    msg.From,
		Date:    timeutil.FromMs(msg.Date).Format("02.01.2006 15:04"),
	}

	// Extract sender info
	result.FromEmail = extractEmailAddress(msg.From)
	result.FromName = extractDisplayName(msg.From)
	if parts := strings.SplitN(result.FromEmail, "@", 2); len(parts) == 2 {
		result.Domain = strings.ToLower(parts[1])
	}

	// 1. Content Analysis
	s.analyzeContent(result, msg)

	// 2. Sender Analysis
	s.analyzeSender(result, msg)

	// 3. Link Analysis
	s.analyzeLinks(result, msg)

	// 4. History Analysis
	s.analyzeHistory(result, userID)

	// 5. Header Analysis
	s.analyzeHeaders(result, msg)

	// Calculate total score
	result.TotalScore = result.ContentScore + result.SenderScore + result.LinkScore + result.HistoryScore

	return result
}

// analyzeContent checks message content for spam indicators
func (s *Server) analyzeContent(result *SpamAnalysisResult, msg *models.Message) {
	// Include both body and body_html for analysis
	content := strings.ToLower(msg.Subject + " " + msg.Body + " " + stripHTML(msg.BodyHTML))
	subjectLower := strings.ToLower(msg.Subject)

	// Check for emojis in subject (common spam indicator)
	emojiCount := countEmojis(msg.Subject)
	if emojiCount > 0 {
		score := float64(emojiCount) * 0.5
		if score > 2.0 {
			score = 2.0
		}
		result.ContentScore += score
		result.Issues = append(result.Issues, SpamIssue{
			Category: "content",
			Severity: "medium",
			Score:    score,
			Message:  fmt.Sprintf("Emojis in subject line: %d found", emojiCount),
		})
	}

	// Russian promotional/spam words (high priority)
	foundRussianSpam := []string{}
	for _, word := range russianSpamWords {
		if strings.Contains(content, word) {
			foundRussianSpam = append(foundRussianSpam, word)
		}
	}
	if len(foundRussianSpam) > 0 {
		score := float64(len(foundRussianSpam)) * 0.7
		if score > 4.0 {
			score = 4.0
		}
		result.ContentScore += score
		result.Issues = append(result.Issues, SpamIssue{
			Category: "content",
			Severity: severityByScore(score),
			Score:    score,
			Message:  fmt.Sprintf("Russian spam words: %s", strings.Join(foundRussianSpam, ", ")),
		})
	}

	// Spam words (English)
	spamWords := []string{
		"viagra", "cialis", "lottery", "winner", "nigerian prince",
		"free money", "act now", "limited time", "click here",
		"you have been selected", "congratulations", "100% free",
		"no cost", "risk free", "guaranteed", "urgent",
	}

	foundWords := []string{}
	for _, word := range spamWords {
		if strings.Contains(content, word) {
			foundWords = append(foundWords, word)
		}
	}

	if len(foundWords) > 0 {
		score := float64(len(foundWords)) * 0.5
		if score > 3.0 {
			score = 3.0
		}
		result.ContentScore += score
		result.Issues = append(result.Issues, SpamIssue{
			Category: "content",
			Severity: severityByScore(score),
			Score:    score,
			Message:  fmt.Sprintf("Spam words found: %s", strings.Join(foundWords, ", ")),
		})
	}

	// Check for percentage/discount patterns in subject
	discountPattern := regexp.MustCompile(`\d+\s*%`)
	if discountPattern.MatchString(subjectLower) {
		result.ContentScore += 1.0
		result.Issues = append(result.Issues, SpamIssue{
			Category: "content",
			Severity: "medium",
			Score:    1.0,
			Message:  "Discount percentage in subject line",
		})
	}

	// Excessive caps in subject
	if len(msg.Subject) > 10 {
		upperCount := 0
		for _, r := range msg.Subject {
			if r >= 'A' && r <= 'Z' || r >= 'А' && r <= 'Я' {
				upperCount++
			}
		}
		if float64(upperCount)/float64(len(msg.Subject)) > 0.5 {
			result.ContentScore += 1.0
			result.Issues = append(result.Issues, SpamIssue{
				Category: "content",
				Severity: "medium",
				Score:    1.0,
				Message:  "Excessive CAPS in subject line",
			})
		}
	}

	// HTML-only message (skip for known brands - they often send HTML-only)
	if msg.BodyHTML != "" && msg.Body == "" && !isKnownBrandDomain(result.Domain) {
		result.ContentScore += 0.5
		result.Issues = append(result.Issues, SpamIssue{
			Category: "content",
			Severity: "low",
			Score:    0.5,
			Message:  "HTML-only message (no plain text)",
		})
	}

	// Check for urgency patterns
	urgencyPatterns := []string{
		"act now", "urgent", "immediately", "expires", "last chance",
		"срочно", "немедленно", "истекает", "последний шанс",
	}
	for _, pattern := range urgencyPatterns {
		if strings.Contains(content, pattern) {
			result.ContentScore += 0.5
			result.Issues = append(result.Issues, SpamIssue{
				Category: "content",
				Severity: "low",
				Score:    0.5,
				Message:  fmt.Sprintf("Urgency pattern: \"%s\"", pattern),
			})
			break
		}
	}
}

// analyzeSender checks sender for spam indicators
func (s *Server) analyzeSender(result *SpamAnalysisResult, msg *models.Message) {
	nameLower := strings.ToLower(result.FromName)
	domainLower := strings.ToLower(result.Domain)

	// Check for brand impersonation (most important check)
	for brand, legitimateDomains := range knownBrands {
		if strings.Contains(nameLower, brand) {
			// Display name contains a known brand - check if domain is legitimate
			isLegitimate := false
			for _, legitDomain := range legitimateDomains {
				if domainLower == legitDomain || strings.HasSuffix(domainLower, "."+legitDomain) {
					isLegitimate = true
					break
				}
			}
			if !isLegitimate {
				result.SenderScore += 5.0
				result.NameEmailMismatch = true
				result.Issues = append(result.Issues, SpamIssue{
					Category: "sender",
					Severity: "high",
					Score:    5.0,
					Message:  fmt.Sprintf("Brand impersonation: \"%s\" but email from %s (expected: %s)", result.FromName, result.Domain, strings.Join(legitimateDomains, " or ")),
				})
				break
			}
		}
	}

	// Check if free email provider with brand name
	if freeEmailProviders[result.Domain] {
		result.IsFreeEmailProvider = true
		for _, brand := range suspiciousBrandNames {
			if strings.Contains(nameLower, brand) {
				result.SenderScore += 3.0
				result.Issues = append(result.Issues, SpamIssue{
					Category: "sender",
					Severity: "high",
					Score:    3.0,
					Message:  fmt.Sprintf("Brand name \"%s\" from free email provider %s", result.FromName, result.Domain),
				})
				break
			}
		}
	}

	// Check for random/generated domain patterns
	if isRandomDomain(result.Domain) {
		result.SenderScore += 2.0
		result.Issues = append(result.Issues, SpamIssue{
			Category: "sender",
			Severity: "medium",
			Score:    2.0,
			Message:  fmt.Sprintf("Suspicious domain pattern: %s (looks randomly generated)", result.Domain),
		})
	}

	// Check for scam-like sender names (e.g., "Лаборатория дохода", "Академия заработка")
	scamNamePatterns := []string{
		"лаборатория", "академия", "институт", "центр", "школа", "клуб",
		"система", "платформа", "проект", "команда", "сообщество",
	}
	scamNameKeywords := []string{
		"доход", "заработ", "прибыл", "инвест", "капитал", "финанс",
		"крипт", "трейд", "торгов", "бизнес", "успех", "богат",
	}
	for _, pattern := range scamNamePatterns {
		if strings.Contains(nameLower, pattern) {
			for _, keyword := range scamNameKeywords {
				if strings.Contains(nameLower, keyword) {
					result.SenderScore += 3.0
					result.Issues = append(result.Issues, SpamIssue{
						Category: "sender",
						Severity: "high",
						Score:    3.0,
						Message:  fmt.Sprintf("Scam-like sender name: \"%s\" (generic org + financial terms)", result.FromName),
					})
					break
				}
			}
			break
		}
	}

	// Check Reply-To mismatch
	if msg.ReplyTo != "" {
		replyToEmail := extractEmailAddress(msg.ReplyTo)
		if replyToEmail != "" && replyToEmail != result.FromEmail {
			replyToDomain := ""
			if parts := strings.SplitN(replyToEmail, "@", 2); len(parts) == 2 {
				replyToDomain = parts[1]
			}
			if replyToDomain != result.Domain {
				result.ReplyToMismatch = true
				result.SenderScore += 1.5
				result.Issues = append(result.Issues, SpamIssue{
					Category: "sender",
					Severity: "medium",
					Score:    1.5,
					Message:  fmt.Sprintf("Reply-To domain (%s) differs from sender domain (%s)", replyToDomain, result.Domain),
				})
			}
		}
	}

	// Check for suspicious display name patterns
	if result.FromName != "" && result.FromEmail != "" {
		// Name contains email-like pattern but different from actual email
		if strings.Contains(result.FromName, "@") {
			nameEmail := extractEmailAddress(result.FromName)
			if nameEmail != "" && nameEmail != result.FromEmail {
				result.SenderScore += 2.0
				result.Issues = append(result.Issues, SpamIssue{
					Category: "sender",
					Severity: "high",
					Score:    2.0,
					Message:  fmt.Sprintf("Display name contains different email: %s", result.FromName),
				})
			}
		}
	}
}

// isRandomDomain checks if a domain looks randomly generated
func isRandomDomain(domain string) bool {
	// Remove TLD
	parts := strings.Split(domain, ".")
	if len(parts) < 2 {
		return false
	}
	name := parts[0]
	if len(parts) > 2 {
		name = parts[len(parts)-2] // Get main domain part
	}

	// Check for random patterns
	// 1. Contains mix of consonants without vowels (Russian-style random)
	// 2. Contains hyphens with random-looking parts
	// 3. Unusual length patterns

	if len(name) > 12 {
		// Long domain names are suspicious
		vowels := 0
		consonants := 0
		for _, r := range strings.ToLower(name) {
			if r == 'a' || r == 'e' || r == 'i' || r == 'o' || r == 'u' {
				vowels++
			} else if r >= 'a' && r <= 'z' {
				consonants++
			}
		}
		// Very low vowel ratio is suspicious
		if vowels > 0 && consonants > 0 {
			ratio := float64(vowels) / float64(vowels+consonants)
			if ratio < 0.15 {
				return true
			}
		}
	}

	// Check for hyphenated random patterns like "rusege-oleneva"
	if strings.Contains(name, "-") {
		hyphenParts := strings.Split(name, "-")
		// Multiple short hyphenated parts that look random
		randomLooking := 0
		for _, part := range hyphenParts {
			if len(part) >= 4 && len(part) <= 8 {
				// Check if it's not a common word
				if !isCommonWord(part) {
					randomLooking++
				}
			}
		}
		if randomLooking >= 2 {
			return true
		}
	}

	return false
}

// isCommonWord checks if a string is a common word
func isCommonWord(s string) bool {
	commonWords := map[string]bool{
		"shop": true, "store": true, "mail": true, "web": true, "info": true,
		"online": true, "digital": true, "tech": true, "cloud": true, "data": true,
		"help": true, "support": true, "sales": true, "news": true, "blog": true,
	}
	return commonWords[strings.ToLower(s)]
}

// isKnownBrandDomain checks if a domain belongs to a known brand
func isKnownBrandDomain(domain string) bool {
	domain = strings.ToLower(domain)
	for _, legitDomains := range knownBrands {
		for _, d := range legitDomains {
			if domain == d || strings.HasSuffix(domain, "."+d) {
				return true
			}
		}
	}
	return false
}

// analyzeLinks checks URLs in message
func (s *Server) analyzeLinks(result *SpamAnalysisResult, msg *models.Message) {
	// Extract URLs
	urlRegex := regexp.MustCompile(`https?://[^\s<>"']+`)
	urls := urlRegex.FindAllString(msg.Body+" "+msg.BodyHTML, -1)

	result.Links = urls

	shorteners := []string{
		"bit.ly", "tinyurl.com", "t.co", "goo.gl", "ow.ly",
		"is.gd", "buff.ly", "adf.ly", "bl.ink", "lnkd.in",
		"shorturl.at", "cutt.ly", "clck.ru", "vk.cc",
	}

	// Check if sender is from a known brand (skip certain checks for legitimate brands)
	senderIsKnownBrand := isKnownBrandDomain(result.Domain)

	trackingURLs := 0
	randomDomainURLs := 0
	brandMismatchURLs := 0

	for _, u := range urls {
		uLower := strings.ToLower(u)
		urlDomain := extractDomainFromURL(u)

		// Check for shorteners (always suspicious)
		for _, shortener := range shorteners {
			if strings.Contains(uLower, shortener) {
				result.ShortenerURLs = append(result.ShortenerURLs, u)
				break
			}
		}

		// Check for suspicious patterns (login/verify from external domains)
		if strings.Contains(uLower, "login") || strings.Contains(uLower, "signin") ||
			strings.Contains(uLower, "verify") || strings.Contains(uLower, "account") ||
			strings.Contains(uLower, "secure") || strings.Contains(uLower, "update") {
			if !strings.Contains(uLower, result.Domain) && !isKnownBrandDomain(urlDomain) {
				result.SuspiciousURLs = append(result.SuspiciousURLs, u)
			}
		}

		// Check for tracking URLs - skip if URL domain is a known brand
		if isTrackingURL(u) && !isKnownBrandDomain(urlDomain) {
			trackingURLs++
		}

		// Check for random-looking domains - skip if it's a known brand
		if urlDomain != "" && !isKnownBrandDomain(urlDomain) && isRandomDomain(urlDomain) {
			randomDomainURLs++
		}

		// Check if URL domain doesn't match claimed brand in sender name
		// Only if sender is NOT from a known brand domain
		if !senderIsKnownBrand && urlDomain != "" && urlDomain != result.Domain {
			nameLower := strings.ToLower(result.FromName)
			for brand, legitDomains := range knownBrands {
				if strings.Contains(nameLower, brand) {
					// Sender claims to be this brand, check if URL is legitimate
					isLegit := false
					for _, d := range legitDomains {
						if urlDomain == d || strings.HasSuffix(urlDomain, "."+d) {
							isLegit = true
							break
						}
					}
					if !isLegit {
						brandMismatchURLs++
					}
					break
				}
			}
		}
	}

	if len(result.ShortenerURLs) > 0 {
		score := float64(len(result.ShortenerURLs)) * 0.5
		if score > 2.0 {
			score = 2.0
		}
		result.LinkScore += score
		result.Issues = append(result.Issues, SpamIssue{
			Category: "link",
			Severity: "medium",
			Score:    score,
			Message:  fmt.Sprintf("URL shorteners found: %d", len(result.ShortenerURLs)),
		})
	}

	if len(result.SuspiciousURLs) > 0 {
		score := float64(len(result.SuspiciousURLs)) * 1.5
		if score > 4.0 {
			score = 4.0
		}
		result.LinkScore += score
		result.Issues = append(result.Issues, SpamIssue{
			Category: "link",
			Severity: "high",
			Score:    score,
			Message:  fmt.Sprintf("Suspicious URLs (login/verify/account) from external domains: %d", len(result.SuspiciousURLs)),
		})
	}

	if trackingURLs > 0 {
		score := float64(trackingURLs) * 1.0
		if score > 3.0 {
			score = 3.0
		}
		result.LinkScore += score
		result.Issues = append(result.Issues, SpamIssue{
			Category: "link",
			Severity: "medium",
			Score:    score,
			Message:  fmt.Sprintf("Tracking URLs with encoded parameters: %d", trackingURLs),
		})
	}

	if randomDomainURLs > 0 {
		score := float64(randomDomainURLs) * 1.5
		if score > 3.0 {
			score = 3.0
		}
		result.LinkScore += score
		result.Issues = append(result.Issues, SpamIssue{
			Category: "link",
			Severity: "high",
			Score:    score,
			Message:  fmt.Sprintf("URLs to random/generated domains: %d", randomDomainURLs),
		})
	}

	if brandMismatchURLs > 0 {
		score := float64(brandMismatchURLs) * 2.0
		if score > 4.0 {
			score = 4.0
		}
		result.LinkScore += score
		result.Issues = append(result.Issues, SpamIssue{
			Category: "link",
			Severity: "high",
			Score:    score,
			Message:  fmt.Sprintf("URLs don't match claimed brand: %d (phishing indicator)", brandMismatchURLs),
		})
	}

	if len(urls) > 10 {
		result.LinkScore += 1.0
		result.Issues = append(result.Issues, SpamIssue{
			Category: "link",
			Severity: "low",
			Score:    1.0,
			Message:  fmt.Sprintf("Excessive number of links: %d", len(urls)),
		})
	}
}

// isTrackingURL checks if URL looks like a tracking/redirect URL
func isTrackingURL(u string) bool {
	// Check for base64-like patterns in query params
	if idx := strings.Index(u, "?"); idx != -1 {
		params := u[idx+1:]
		// Base64 pattern: long alphanumeric strings with = padding
		base64Pattern := regexp.MustCompile(`[A-Za-z0-9+/]{20,}={0,2}`)
		if base64Pattern.MatchString(params) {
			return true
		}
	}

	// Check for long random hash in path (like /c/91822064556776O6T6I132H9)
	pathPattern := regexp.MustCompile(`/[a-zA-Z0-9]{15,}`)
	if pathPattern.MatchString(u) {
		return true
	}

	return false
}

// extractDomainFromURL extracts domain from URL
func extractDomainFromURL(u string) string {
	// Simple extraction: get part between :// and first /
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	if idx := strings.Index(u, "/"); idx != -1 {
		u = u[:idx]
	}
	if idx := strings.Index(u, "?"); idx != -1 {
		u = u[:idx]
	}
	// Remove port if present
	if idx := strings.Index(u, ":"); idx != -1 {
		u = u[:idx]
	}
	return strings.ToLower(u)
}

// stripHTML removes HTML tags and returns plain text
func stripHTML(htmlContent string) string {
	// Remove script and style content
	scriptRegex := regexp.MustCompile(`(?i)<script[^>]*>[\s\S]*?</script>`)
	htmlContent = scriptRegex.ReplaceAllString(htmlContent, "")
	styleRegex := regexp.MustCompile(`(?i)<style[^>]*>[\s\S]*?</style>`)
	htmlContent = styleRegex.ReplaceAllString(htmlContent, "")

	// Remove HTML tags
	tagRegex := regexp.MustCompile(`<[^>]*>`)
	text := tagRegex.ReplaceAllString(htmlContent, " ")

	// Decode common HTML entities
	text = strings.ReplaceAll(text, "&nbsp;", " ")
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&quot;", "\"")
	text = strings.ReplaceAll(text, "&#39;", "'")
	text = strings.ReplaceAll(text, "₽", "руб")

	// Collapse whitespace
	spaceRegex := regexp.MustCompile(`\s+`)
	text = spaceRegex.ReplaceAllString(text, " ")

	return strings.TrimSpace(text)
}

// countEmojis counts emoji characters in a string
func countEmojis(s string) int {
	count := 0
	for _, r := range s {
		// Common emoji ranges
		if (r >= 0x1F300 && r <= 0x1F9FF) || // Misc Symbols, Emoticons, etc
			(r >= 0x2600 && r <= 0x26FF) || // Misc symbols
			(r >= 0x2700 && r <= 0x27BF) || // Dingbats
			(r >= 0x1F600 && r <= 0x1F64F) || // Emoticons
			(r >= 0x1F680 && r <= 0x1F6FF) || // Transport symbols
			(r >= 0x1F1E0 && r <= 0x1F1FF) { // Flags
			count++
		}
	}
	return count
}

// analyzeHistory checks sender history
func (s *Server) analyzeHistory(result *SpamAnalysisResult, userID int64) {
	if result.FromEmail == "" {
		return
	}

	// Get sender stats
	stats, err := s.database.GetSenderStats(userID, result.FromEmail, result.Domain)
	if err != nil {
		log.Printf("Failed to get sender stats: %v", err)
		return
	}
	result.SenderStats = stats

	if stats.HasWhitelist {
		result.HasWhitelist = true
		result.HistoryScore -= 5.0 // Bonus for whitelist
		result.Issues = append(result.Issues, SpamIssue{
			Category: "history",
			Severity: "low",
			Score:    -5.0,
			Message:  "Sender is in your whitelist",
		})
	}

	if stats.HasBlacklist {
		result.HasBlacklist = true
		result.HistoryScore += 5.0
		result.Issues = append(result.Issues, SpamIssue{
			Category: "history",
			Severity: "high",
			Score:    5.0,
			Message:  "Sender is in your blacklist",
		})
	}

	// Check spam ratio
	if stats.TotalMessages > 0 {
		spamRatio := float64(stats.SpamMessages) / float64(stats.TotalMessages)
		if spamRatio > 0.5 && stats.SpamMessages >= 2 {
			result.HistoryScore += 2.0
			result.Issues = append(result.Issues, SpamIssue{
				Category: "history",
				Severity: "high",
				Score:    2.0,
				Message: fmt.Sprintf("High spam ratio from sender: %d/%d messages marked as spam (%.0f%%)",
					stats.SpamMessages, stats.TotalMessages, spamRatio*100),
			})
		}
	}

	// Check domain stats
	domainTotal, domainSpam, err := s.database.GetDomainStats(userID, result.Domain)
	if err == nil {
		result.DomainTotal = domainTotal
		result.DomainSpam = domainSpam

		if domainTotal > 5 {
			spamRatio := float64(domainSpam) / float64(domainTotal)
			if spamRatio > 0.5 {
				result.HistoryScore += 1.5
				result.Issues = append(result.Issues, SpamIssue{
					Category: "history",
					Severity: "medium",
					Score:    1.5,
					Message: fmt.Sprintf("High spam ratio from domain @%s: %d/%d messages (%.0f%%)",
						result.Domain, domainSpam, domainTotal, spamRatio*100),
				})
			}
		}
	}

	// First message from sender (new sender)
	if stats.TotalMessages == 1 {
		result.Issues = append(result.Issues, SpamIssue{
			Category: "history",
			Severity: "low",
			Score:    0,
			Message:  "First message from this sender",
		})
	}
}

// analyzeHeaders checks message headers
func (s *Server) analyzeHeaders(result *SpamAnalysisResult, msg *models.Message) {
	// Check if subject is empty
	if strings.TrimSpace(msg.Subject) == "" {
		result.ContentScore += 0.5
		result.Issues = append(result.Issues, SpamIssue{
			Category: "header",
			Severity: "low",
			Score:    0.5,
			Message:  "Empty subject line",
		})
	}

	// Check for Re:/Fwd: in subject without In-Reply-To
	if (strings.HasPrefix(msg.Subject, "Re:") || strings.HasPrefix(msg.Subject, "Fwd:")) &&
		msg.InReplyTo == "" {
		result.ContentScore += 0.5
		result.Issues = append(result.Issues, SpamIssue{
			Category: "header",
			Severity: "low",
			Score:    0.5,
			Message:  "Reply/Forward subject without In-Reply-To header (fake reply)",
		})
	}
}

// renderSpamAnalysisResult renders the analysis result as HTML
func (s *Server) renderSpamAnalysisResult(w http.ResponseWriter, result *SpamAnalysisResult, messageID string) {
	var b strings.Builder

	// Main card
	b.WriteString(`<div class="card">`)
	b.WriteString(`<div class="card-header">`)
	b.WriteString(`<h4 class="card-title">`)
	b.WriteString(html.EscapeString(result.Subject))
	b.WriteString(`</h4>`)
	b.WriteString(`</div>`)
	b.WriteString(`<div class="card-body">`)

	// Message info
	b.WriteString(`<div class="row mb-4">`)
	b.WriteString(`<div class="col-md-6">`)
	b.WriteString(`<dl>`)
	b.WriteString(`<dt>From</dt><dd><code>`)
	b.WriteString(html.EscapeString(result.From))
	b.WriteString(`</code></dd>`)
	b.WriteString(`<dt>Date</dt><dd>`)
	b.WriteString(result.Date)
	b.WriteString(`</dd>`)
	b.WriteString(`</dl>`)
	b.WriteString(`</div>`)

	// Score summary
	b.WriteString(`<div class="col-md-6">`)
	b.WriteString(`<div class="card`)
	if result.TotalScore >= 6.0 {
		b.WriteString(` bg-red-lt`)
	} else if result.TotalScore >= 3.0 {
		b.WriteString(` bg-orange-lt`)
	} else {
		b.WriteString(` bg-green-lt`)
	}
	b.WriteString(`">`)
	b.WriteString(`<div class="card-body text-center">`)
	b.WriteString(`<div class="display-6 fw-bold">`)
	b.WriteString(fmt.Sprintf("%.1f", result.TotalScore))
	b.WriteString(`</div>`)
	b.WriteString(`<div class="text-muted">Total Spam Score</div>`)
	b.WriteString(`</div></div></div></div>`)

	// Score breakdown
	b.WriteString(`<div class="row mb-4">`)
	b.WriteString(`<div class="col-md-3"><div class="card card-sm"><div class="card-body">`)
	b.WriteString(fmt.Sprintf(`<div class="fw-bold">%.1f</div><div class="text-muted small">Content</div>`, result.ContentScore))
	b.WriteString(`</div></div></div>`)
	b.WriteString(`<div class="col-md-3"><div class="card card-sm"><div class="card-body">`)
	b.WriteString(fmt.Sprintf(`<div class="fw-bold">%.1f</div><div class="text-muted small">Sender</div>`, result.SenderScore))
	b.WriteString(`</div></div></div>`)
	b.WriteString(`<div class="col-md-3"><div class="card card-sm"><div class="card-body">`)
	b.WriteString(fmt.Sprintf(`<div class="fw-bold">%.1f</div><div class="text-muted small">Links</div>`, result.LinkScore))
	b.WriteString(`</div></div></div>`)
	b.WriteString(`<div class="col-md-3"><div class="card card-sm"><div class="card-body">`)
	b.WriteString(fmt.Sprintf(`<div class="fw-bold">%.1f</div><div class="text-muted small">History</div>`, result.HistoryScore))
	b.WriteString(`</div></div></div>`)
	b.WriteString(`</div>`)

	// Issues list
	if len(result.Issues) > 0 {
		b.WriteString(`<h5>Detected Issues</h5>`)
		b.WriteString(`<table class="table table-vcenter mb-4">`)
		b.WriteString(`<thead><tr><th>Issue</th><th class="text-end" style="width:100px">Category</th><th class="text-end" style="width:80px">Score</th></tr></thead>`)
		b.WriteString(`<tbody>`)
		for _, issue := range result.Issues {
			badgeClass := "bg-blue"
			icon := "ti-info-circle"
			switch issue.Severity {
			case "high":
				badgeClass = "bg-red"
				icon = "ti-alert-circle"
			case "medium":
				badgeClass = "bg-orange"
				icon = "ti-alert-triangle"
			case "low":
				badgeClass = "bg-yellow"
				icon = "ti-info-circle"
			}
			b.WriteString(`<tr>`)
			b.WriteString(`<td><i class="ti `)
			b.WriteString(icon)
			b.WriteString(` me-2"></i>`)
			b.WriteString(html.EscapeString(issue.Message))
			b.WriteString(`</td>`)
			b.WriteString(fmt.Sprintf(`<td class="text-end"><span class="badge %s">%s</span></td>`, badgeClass, issue.Category))
			scoreClass := "text-danger fw-bold"
			if issue.Score < 0 {
				scoreClass = "text-success fw-bold"
			} else if issue.Score < 1 {
				scoreClass = "text-warning"
			}
			b.WriteString(fmt.Sprintf(`<td class="text-end %s">%+.1f</td>`, scoreClass, issue.Score))
			b.WriteString(`</tr>`)
		}
		b.WriteString(`</tbody></table>`)
	} else {
		b.WriteString(`<div class="alert alert-success mb-4">`)
		b.WriteString(`<i class="ti ti-check me-2"></i>No spam indicators detected`)
		b.WriteString(`</div>`)
	}

	// Sender statistics
	if result.SenderStats != nil && result.SenderStats.TotalMessages > 0 {
		b.WriteString(`<h5>Sender Statistics</h5>`)
		b.WriteString(`<div class="row mb-4">`)
		b.WriteString(`<div class="col-md-6">`)
		b.WriteString(`<table class="table table-sm">`)
		b.WriteString(`<tr><td>Messages from sender</td><td class="text-end"><strong>`)
		b.WriteString(fmt.Sprintf("%d", result.SenderStats.TotalMessages))
		b.WriteString(`</strong></td></tr>`)
		b.WriteString(`<tr><td>Marked as spam</td><td class="text-end"><strong>`)
		b.WriteString(fmt.Sprintf("%d", result.SenderStats.SpamMessages))
		b.WriteString(`</strong></td></tr>`)
		if result.SenderStats.FirstMessageAt != 0 {
			b.WriteString(`<tr><td>First message</td><td class="text-end">`)
			b.WriteString(timeutil.FromMs(result.SenderStats.FirstMessageAt).Format("02.01.2006"))
			b.WriteString(`</td></tr>`)
		}
		b.WriteString(`</table></div>`)

		b.WriteString(`<div class="col-md-6">`)
		b.WriteString(`<table class="table table-sm">`)
		b.WriteString(`<tr><td>Messages from @`)
		b.WriteString(html.EscapeString(result.Domain))
		b.WriteString(`</td><td class="text-end"><strong>`)
		b.WriteString(fmt.Sprintf("%d", result.DomainTotal))
		b.WriteString(`</strong></td></tr>`)
		b.WriteString(`<tr><td>Domain spam</td><td class="text-end"><strong>`)
		b.WriteString(fmt.Sprintf("%d", result.DomainSpam))
		b.WriteString(`</strong></td></tr>`)
		b.WriteString(`</table></div>`)
		b.WriteString(`</div>`)
	}

	// Links info
	if len(result.Links) > 0 {
		b.WriteString(`<details class="mb-4"><summary class="mb-2">`)
		b.WriteString(fmt.Sprintf(`Links found (%d)`, len(result.Links)))
		b.WriteString(`</summary>`)
		b.WriteString(`<div class="small">`)
		for i, link := range result.Links {
			if i >= 10 {
				b.WriteString(fmt.Sprintf(`<div class="text-muted">... and %d more</div>`, len(result.Links)-10))
				break
			}
			isShortener := false
			for _, s := range result.ShortenerURLs {
				if s == link {
					isShortener = true
					break
				}
			}
			isSuspicious := false
			for _, s := range result.SuspiciousURLs {
				if s == link {
					isSuspicious = true
					break
				}
			}
			b.WriteString(`<div class="text-truncate`)
			if isShortener {
				b.WriteString(` text-warning`)
			}
			if isSuspicious {
				b.WriteString(` text-danger`)
			}
			b.WriteString(`"><code>`)
			b.WriteString(html.EscapeString(link))
			b.WriteString(`</code>`)
			if isShortener {
				b.WriteString(` <span class="badge bg-warning">shortener</span>`)
			}
			if isSuspicious {
				b.WriteString(` <span class="badge bg-danger">suspicious</span>`)
			}
			b.WriteString(`</div>`)
		}
		b.WriteString(`</div></details>`)
	}

	// Actions
	b.WriteString(`<h5>Actions</h5>`)
	b.WriteString(`<div class="btn-list">`)

	// Mark as spam
	b.WriteString(fmt.Sprintf(`<button class="btn btn-warning" hx-post="/spam/mark-by-message-id" hx-vals='{"message_id":"%s","create_rule":"0"}' hx-swap="none" hx-on::after-request="location.reload()">`,
		html.EscapeString(messageID)))
	b.WriteString(`<i class="ti ti-shield-x me-1"></i>Mark as Spam</button>`)

	// Block sender
	if result.FromEmail != "" && !result.HasBlacklist {
		b.WriteString(fmt.Sprintf(`<button class="btn btn-outline-danger" hx-post="/spam/rules" hx-vals='{"rule_type":"address","rule_value":"%s","action":"spam"}' hx-swap="none" hx-on::after-request="location.reload()">`,
			html.EscapeString(result.FromEmail)))
		b.WriteString(`<i class="ti ti-user-x me-1"></i>Block Sender</button>`)
	}

	// Block domain
	if result.Domain != "" {
		b.WriteString(fmt.Sprintf(`<button class="btn btn-outline-danger" hx-post="/spam/rules" hx-vals='{"rule_type":"domain","rule_value":"%s","action":"spam"}' hx-swap="none" hx-on::after-request="location.reload()">`,
			html.EscapeString(result.Domain)))
		b.WriteString(fmt.Sprintf(`<i class="ti ti-world-x me-1"></i>Block @%s</button>`, html.EscapeString(result.Domain)))
	}

	// Whitelist sender
	if result.FromEmail != "" && !result.HasWhitelist {
		b.WriteString(fmt.Sprintf(`<button class="btn btn-outline-success" hx-post="/spam/rules" hx-vals='{"rule_type":"address","rule_value":"%s","action":"allow"}' hx-swap="none" hx-on::after-request="location.reload()">`,
			html.EscapeString(result.FromEmail)))
		b.WriteString(`<i class="ti ti-user-check me-1"></i>Whitelist Sender</button>`)
	}

	b.WriteString(`</div>`)
	b.WriteString(`</div></div>`)

	w.Write([]byte(b.String()))
}

// extractDisplayName extracts display name from "Name <email>" format
func extractDisplayName(from string) string {
	from = strings.TrimSpace(from)
	if idx := strings.Index(from, "<"); idx > 0 {
		return strings.TrimSpace(from[:idx])
	}
	return ""
}

// severityByScore returns severity level by score
func severityByScore(score float64) string {
	if score >= 2.0 {
		return "high"
	} else if score >= 1.0 {
		return "medium"
	}
	return "low"
}

// extractEmailAddress extracts email from formats like "Name <email@example.com>"
func extractEmailAddress(from string) string {
	from = strings.TrimSpace(from)
	if from == "" {
		return ""
	}

	// Check for angle bracket format
	start := strings.Index(from, "<")
	end := strings.Index(from, ">")
	if start >= 0 && end > start {
		return strings.ToLower(strings.TrimSpace(from[start+1 : end]))
	}

	// Assume it's just the email
	return strings.ToLower(from)
}
