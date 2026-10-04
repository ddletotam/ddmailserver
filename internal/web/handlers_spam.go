package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/notify"
	"github.com/ddletotam/ddmailserver/internal/parser"
	spamsvc "github.com/ddletotam/ddmailserver/internal/service/spam"
	"github.com/gorilla/mux"
)

// publishInboxUpdate nudges IMAP IDLE / WebSocket subscribers that the user's
// INBOX changed (one or more restored messages). Idempotent — no-op when the
// hub isn't wired up (e.g. tests). Uses count=1 even for bulk because the
// listeners only care that "something changed", not how much.
func (s *Server) publishInboxUpdate(user *models.User, count uint32) {
	if s.notifyHub == nil || user == nil {
		return
	}
	if count == 0 {
		count = 1
	}
	s.notifyHub.Publish(notify.Event{
		UserID:   user.ID,
		Type:     notify.EventNewMessage,
		Username: user.Username,
		Mailbox:  "INBOX",
		Count:    count,
	})
}

// SpamRow is the per-message view-model rendered in the spam table. It
// pre-decodes the reason list (stored as JSON in the DB), splits the From
// header into display name + bare address + domain, and trims the body for a
// preview so the template stays declarative.
type SpamRow struct {
	*models.Message
	Reasons     []string
	BodyPreview string
	FromName    string
	FromAddress string
	FromDomain  string
}

// SpamData holds data for the spam page
type SpamData struct {
	PageData
	Rows         []SpamRow
	MessageCount int
	TotalCount   int
	Page         int
	PageSize     int
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

// SpamCheck represents a spam check that can be enabled/disabled. Weight is
// the per-user multiplier (1.0 = stock); the UI lets the user override it.
type SpamCheck struct {
	Name        string
	Description string
	Enabled     bool
	Weight      float64
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

	rows := buildSpamRows(messages)

	// Partial render for infinite scroll — htmx fetches the next page and
	// appends just the <tr> rows, no chrome.
	if r.URL.Query().Get("partial") == "1" {
		partial := SpamData{
			PageData:     PageData{User: user},
			Rows:         rows,
			MessageCount: len(rows),
			TotalCount:   total,
			Page:         page,
			PageSize:     pageSize,
			HasNextPage:  page*pageSize < total,
		}
		s.renderTemplatePartial(w, "spam.html", "spam-rows", partial)
		return
	}

	data := SpamData{
		PageData: PageData{
			Title: i18n.T("spam.title"),
			User:  user,
		},
		Rows:         rows,
		MessageCount: len(rows),
		TotalCount:   total,
		Page:         page,
		PageSize:     pageSize,
		HasNextPage:  page*pageSize < total,
	}

	s.renderTemplate(w, "spam.html", data)
}

// buildSpamRows decorates each message with parsed reasons, a body preview,
// and a split sender. Kept in a helper so the partial-render path uses the
// exact same projection.
func buildSpamRows(messages []*models.Message) []SpamRow {
	rows := make([]SpamRow, 0, len(messages))
	for _, m := range messages {
		var reasons []string
		if m.SpamReasons != "" {
			json.Unmarshal([]byte(m.SpamReasons), &reasons)
		}
		addr := strings.ToLower(extractEmailAddress(m.From))
		name := extractName(m.From)
		domain := ""
		if at := strings.LastIndex(addr, "@"); at > 0 && at+1 < len(addr) {
			domain = addr[at+1:]
		}
		preview := strings.TrimSpace(m.Body)
		preview = strings.Join(strings.Fields(preview), " ")
		if len(preview) > 220 {
			preview = preview[:220] + "…"
		}
		rows = append(rows, SpamRow{
			Message:     m,
			Reasons:     reasons,
			BodyPreview: preview,
			FromName:    name,
			FromAddress: addr,
			FromDomain:  domain,
		})
	}
	return rows
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

	// Wake any IMAP IDLE / WebSocket subscribers — without this the desktop
	// client doesn't notice the message reappearing in INBOX until it
	// reconnects.
	s.publishInboxUpdate(user, 1)

	// For htmx, return empty response to remove the row
	w.Header().Set("HX-Trigger", "spamRestored")
	w.WriteHeader(http.StatusOK)
}

// HandleRestoreSpamBySender restores every spam message whose From matches
// the given address. When `allow=1` is passed it also inserts a whitelist
// rule so future deliveries from the same sender skip the filter. Renders
// the refreshed first page of remaining spam rows for htmx to swap in.
func (s *Server) HandleRestoreSpamBySender(w http.ResponseWriter, r *http.Request) {
	s.handleRestoreSpamBulk(w, r, "sender")
}

// HandleRestoreSpamByDomain — domain variant of the above. Matches the host
// part of the From address and supports the same optional whitelist rule.
func (s *Server) HandleRestoreSpamByDomain(w http.ResponseWriter, r *http.Request) {
	s.handleRestoreSpamBulk(w, r, "domain")
}

func (s *Server) handleRestoreSpamBulk(w http.ResponseWriter, r *http.Request, mode string) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var value string
	var ids []int64
	var err error
	switch mode {
	case "sender":
		value = strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sender")))
		if value == "" {
			http.Error(w, "missing sender", http.StatusBadRequest)
			return
		}
		ids, err = s.database.GetSpamMessageIDsBySender(user.ID, value)
	case "domain":
		value = strings.ToLower(strings.TrimSpace(r.URL.Query().Get("domain")))
		if value == "" {
			http.Error(w, "missing domain", http.StatusBadRequest)
			return
		}
		ids, err = s.database.GetSpamMessageIDsByDomain(user.ID, value)
	}
	if err != nil {
		log.Printf("Bulk restore: query failed: %v", err)
		http.Error(w, "Lookup failed", http.StatusInternalServerError)
		return
	}

	restored := 0
	for _, id := range ids {
		if err := s.database.RestoreFromSpam(id, user.ID); err != nil {
			log.Printf("Bulk restore: failed to restore %d: %v", id, err)
			continue
		}
		restored++
	}

	if r.URL.Query().Get("allow") == "1" {
		ruleType := "address"
		if mode == "domain" {
			ruleType = "domain"
		}
		if err := s.database.CreateSpamRule(&db.SpamRule{
			UserID:    user.ID,
			RuleType:  ruleType,
			RuleValue: value,
			Action:    "allow",
		}); err != nil {
			log.Printf("Bulk restore: failed to create allow rule: %v", err)
		}
	}

	log.Printf("Spam bulk restore (%s=%s, allow=%v): restored %d/%d messages",
		mode, value, r.URL.Query().Get("allow") == "1", restored, len(ids))

	if restored > 0 {
		s.publishInboxUpdate(user, uint32(restored))
	}

	// Re-render the first page of spam so htmx swaps the table body with the
	// fresh remainder. The user keeps their place; the dropdown's
	// hx-target points at #spam-list innerHTML.
	s.renderRefreshedSpamRows(w, r, user)
}

func (s *Server) renderRefreshedSpamRows(w http.ResponseWriter, r *http.Request, user *models.User) {
	pageSize := 50
	messages, total, err := s.database.GetSpamMessages(user.ID, pageSize, 0)
	if err != nil {
		log.Printf("Failed to reload spam messages: %v", err)
	}
	data := SpamData{
		PageData:     PageData{User: user},
		Rows:         buildSpamRows(messages),
		MessageCount: len(messages),
		TotalCount:   total,
		Page:         1,
		PageSize:     pageSize,
		HasNextPage:  pageSize < total,
	}
	s.renderTemplatePartial(w, "spam.html", "spam-rows", data)
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

	// Per-rule check exclusions — only meaningful for allow rules. The form
	// posts each selected check as a separate `excluded_checks` value; the
	// rest of the form ignores it.
	var excluded []string
	if action == "allow" {
		for _, c := range r.Form["excluded_checks"] {
			c = strings.TrimSpace(c)
			if validCheckName(c) {
				excluded = append(excluded, c)
			}
		}
	}

	rule := &db.SpamRule{
		UserID:         user.ID,
		RuleType:       ruleType,
		RuleValue:      strings.ToLower(ruleValue),
		Action:         action,
		ExcludedChecks: excluded,
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

	weights, wErr := s.database.GetSpamCheckWeights(user.ID)
	if wErr != nil {
		log.Printf("Failed to get spam weights: %v", wErr)
		weights = map[string]float64{}
	}
	weightOr := func(name string) float64 {
		if w, ok := weights[name]; ok {
			return w
		}
		return parser.DefaultCategoryWeight(name)
	}

	// One row per analyzer category, straight from parser.SpamCheckCategories
	// so a new category cannot be scored by the analyzer yet missing here.
	// The fine-grained url_shortener toggle lives under links and is exposed
	// only via the per-check disable flow, not weights.
	var availableChecks []SpamCheck
	for _, name := range parser.SpamCheckCategories {
		availableChecks = append(availableChecks, SpamCheck{
			Name: name, Description: i18n.T("spam.check." + name),
			Enabled: !disabledMap[name], Weight: weightOr(name),
		})
	}
	availableChecks = append(availableChecks, SpamCheck{
		Name: "url_shortener", Description: i18n.T("spam.check.url_shortener"),
		Enabled: !disabledMap["url_shortener"], Weight: 1.0,
	})

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

	if !validCheckName(checkName) {
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

// validCheckName is the single source of truth for which check-name strings
// are valid input on the settings page. Includes the analyzer categories
// (parser.SpamCheckCategories) and the two fine-grained sub-toggles that
// `disabledChecks` keys also use.
func validCheckName(name string) bool {
	if name == "url_shortener" {
		return true
	}
	for _, c := range parser.SpamCheckCategories {
		if name == c {
			return true
		}
	}
	return false
}

// HandleSetSpamCheckWeight upserts a per-check weight. POST form field
// `weight` (float, clamped to [0, 10]). Returns 204 on success — htmx can
// just ignore the body.
func (s *Server) HandleSetSpamCheckWeight(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	name := mux.Vars(r)["name"]
	if !validCheckName(name) {
		http.Error(w, "Invalid check name", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}
	weight, err := strconv.ParseFloat(r.FormValue("weight"), 64)
	if err != nil {
		http.Error(w, "Invalid weight", http.StatusBadRequest)
		return
	}
	if err := s.database.SetSpamCheckWeight(user.ID, name, weight); err != nil {
		log.Printf("Failed to set spam check weight: %v", err)
		http.Error(w, "Failed to save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

// HandleAnalyzeByMessageID explains why a message is (or is not) spam. The
// explanation is the verdict stored at delivery plus the delivery analyzer
// re-run (internal/service/spam) — not a second scoring engine of its own.
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

	i18n := s.i18nManager.Get(s.getUserLanguage(map[string]interface{}{"User": user}))
	alert := func(kind, key string) {
		fmt.Fprintf(w, `<div class="alert alert-%s">%s</div>`, kind, html.EscapeString(i18n.T(key)))
	}

	messageID := strings.TrimSpace(r.FormValue("message_id"))
	if messageID == "" {
		alert("danger", "spam.analyze.need_message_id")
		return
	}

	explanation, err := s.spamExplainer().ExplainByMessageID(user.ID, messageID)
	if errors.Is(err, spamsvc.ErrNotFound) {
		alert("warning", "spam.analyze.not_found")
		return
	}
	if err != nil {
		log.Printf("spam explanation for user %d: %v", user.ID, err)
		alert("danger", "spam.analyze.failed")
		return
	}

	s.renderTemplatePartial(w, "spam_analysis.html", "spam-analysis", SpamAnalysisView{
		User:        user,
		E:           explanation,
		MessageID:   messageID,
		CheckLabels: spamCheckLabels(i18n),
	})
}

// SpamAnalysisView is the data of the spam_analysis.html partial.
type SpamAnalysisView struct {
	User        *models.User
	E           *spamsvc.Explanation
	MessageID   string
	CheckLabels map[string]string
}

// UserLanguage makes the partial render in the user's language.
func (v SpamAnalysisView) UserLanguage() string {
	if v.User == nil {
		return ""
	}
	return v.User.Language
}

// HxVals encodes key/value pairs as the JSON object of an hx-vals attribute.
// Built with encoding/json (html/template then escapes it for the
// attribute), so a quote in a Message-ID cannot break out of the JSON.
func (v SpamAnalysisView) HxVals(pairs ...string) (string, error) {
	if len(pairs)%2 != 0 {
		return "", fmt.Errorf("HxVals: odd number of arguments")
	}
	m := make(map[string]string, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// spamCheckLabels maps every category a finding can carry to its label.
func spamCheckLabels(i18n *I18n) map[string]string {
	labels := map[string]string{
		"upstream":  i18n.T("spam.analyze.check_upstream"),
		"recipient": i18n.T("spam.analyze.check_recipient"),
	}
	for _, c := range parser.SpamCheckCategories {
		labels[c] = i18n.T("spam.check." + c)
	}
	return labels
}

// spamExplainer returns the explainer; it re-runs the stock analyzer with the
// network checks off (their answers belong to the moment of delivery).
func (s *Server) spamExplainer() *spamsvc.Explainer {
	return spamsvc.NewExplainer(s.database, nil)
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
