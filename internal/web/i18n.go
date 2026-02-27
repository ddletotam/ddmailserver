package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"log"
)

//go:embed locales/*.json
var localesFS embed.FS

// I18n handles internationalization
type I18n struct {
	locale       string
	translations map[string]string
}

// NewI18n creates a new i18n instance
func NewI18n(locale string) (*I18n, error) {
	i18n := &I18n{
		locale:       locale,
		translations: make(map[string]string),
	}

	if err := i18n.loadLocale(locale); err != nil {
		log.Printf("Failed to load locale %s, falling back to en: %v", locale, err)
		// Fallback to English
		if err := i18n.loadLocale("en"); err != nil {
			return nil, fmt.Errorf("failed to load fallback locale: %w", err)
		}
	}

	log.Printf("Loaded locale: %s (%d translations)", i18n.locale, len(i18n.translations))
	return i18n, nil
}

// loadLocale loads translations from JSON file
func (i *I18n) loadLocale(locale string) error {
	filename := fmt.Sprintf("locales/%s.json", locale)
	data, err := localesFS.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("failed to read locale file %s: %w", filename, err)
	}

	if err := json.Unmarshal(data, &i.translations); err != nil {
		return fmt.Errorf("failed to parse locale file %s: %w", filename, err)
	}

	i.locale = locale
	return nil
}

// T translates a key
func (i *I18n) T(key string) string {
	if translation, ok := i.translations[key]; ok {
		return translation
	}
	// Return key if translation not found
	log.Printf("Translation not found for key: %s", key)
	return key
}

// GetLocale returns current locale
func (i *I18n) GetLocale() string {
	return i.locale
}
