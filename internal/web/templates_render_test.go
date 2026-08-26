package web

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/yourusername/mailserver/internal/models"
)

// TestSettingsScriptEmitsQuotedStrings renders the settings page and checks the
// inline script.
//
// The panels there are plain DOM, and their labels come from `{{t "…"}}`
// actions placed inside JavaScript expressions. html/template is supposed to
// notice the JS context and emit a quoted, escaped literal. If it ever emitted
// the bare text instead, the script would be a syntax error and both panels
// would silently do nothing — a failure no Go test would otherwise catch and
// no template-parse check can see.
func TestSettingsScriptEmitsQuotedStrings(t *testing.T) {
	s := &Server{i18nManager: NewI18nManager()}
	data := map[string]interface{}{
		"User":     &models.User{Username: "tester", Email: "tester@example.org", Language: "en"},
		"Language": "en",
	}

	tmpl, err := template.New("").Funcs(s.buildFuncMap(data)).
		ParseFS(templatesFS, "templates/layout.html", "templates/settings.html")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "content", data); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()

	script := out[strings.Index(out, "<script>"):]

	// A localized label used as a bare JS expression. Quoted means the context
	// was understood; unquoted would be a ReferenceError at best.
	for _, want := range []string{
		`text(ignored, "`,
		`if (!confirm("`,
		`td.textContent = "`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("expected a quoted JS string at %q — the label was not escaped as a JS value", want)
		}
	}

	// The template must not have degraded into HTML-escaped text inside JS.
	if strings.Contains(script, "&#34;") || strings.Contains(script, "&amp;") {
		t.Error("script contains HTML entities — the action was escaped as HTML rather than as a JS value")
	}

	// ZgotmplZ is html/template's marker for a value it refused to interpolate.
	if strings.Contains(out, "ZgotmplZ") {
		t.Error("template produced ZgotmplZ — a value was rejected as unsafe for its context")
	}
}
