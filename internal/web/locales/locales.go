// Package locales carries the translation catalogues (en.json, ru.json)
// inside the binary.
//
// The web UI reads them through web.I18nManager. Code outside the web
// package that writes text for people — the delivery status notification
// the outbox puts into the sender's mailbox — loads a catalogue with Load.
package locales

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
)

//go:embed *.json
var files embed.FS

// FS returns the embedded catalogues, one <locale>.json per locale.
func FS() fs.FS {
	return files
}

// Load returns the translations of locale ("en", "ru").
func Load(locale string) (map[string]string, error) {
	data, err := files.ReadFile(locale + ".json")
	if err != nil {
		return nil, fmt.Errorf("locale %s: %w", locale, err)
	}
	out := make(map[string]string)
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("locale %s: %w", locale, err)
	}
	return out, nil
}
