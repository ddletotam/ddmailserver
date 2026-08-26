package web

import (
	"html/template"
	"io/fs"
	"strings"
	"testing"
)

// TestTemplatesParse catches a malformed template at build time rather than
// when someone opens the page. Every template is rendered through the same
// ParseFS call with the same func map, so a parse error here is exactly the
// error production would hit.
func TestTemplatesParse(t *testing.T) {
	s := &Server{i18nManager: NewI18nManager()}
	funcs := s.buildFuncMap(map[string]interface{}{})

	entries, err := fs.ReadDir(templatesFS, "templates")
	if err != nil {
		t.Fatalf("read templates dir: %v", err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			if _, err := template.New("").Funcs(funcs).
				ParseFS(templatesFS, "templates/layout.html", "templates/"+name); err != nil {
				t.Errorf("%s does not parse: %v", name, err)
			}
		})
	}
}
