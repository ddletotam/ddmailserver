package web

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/parser"
	spamsvc "github.com/ddletotam/ddmailserver/internal/service/spam"
	"github.com/gorilla/mux"
)

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
