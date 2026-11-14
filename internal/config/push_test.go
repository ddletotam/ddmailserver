package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Секции push.hms и push.rustore читаются; без них или с пустыми
// значениями службы выключены, конфиг остаётся допустимым.
func TestPushAndroidSections(t *testing.T) {
	cfg := loadString(t, validBase+`
push:
  hms:
    app_id: "107000001"
    client_secret: "secret"
    mode: notification
  rustore:
    project_id: "proj"
    service_token: "svc"
`)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !cfg.Push.HMS.Enabled() || cfg.Push.HMS.ModeOrDefault() != HMSModeNotification {
		t.Errorf("hms = %+v", cfg.Push.HMS)
	}
	if !cfg.Push.RuStore.Enabled() || cfg.Push.RuStore.ProjectID != "proj" {
		t.Errorf("rustore = %+v", cfg.Push.RuStore)
	}

	empty := loadString(t, validBase+`
push:
  hms:
    app_id: ""
  rustore: {}
`)
	if err := empty.Validate(); err != nil {
		t.Fatalf("empty sections rejected: %v", err)
	}
	if empty.Push.HMS.Enabled() || empty.Push.RuStore.Enabled() {
		t.Error("empty sections enabled a service")
	}
	if empty.Push.HMS.ModeOrDefault() != HMSModeData {
		t.Errorf("default mode = %q", empty.Push.HMS.ModeOrDefault())
	}
	// Половина настроек — служба выключена, а не сломана.
	half := RuStoreConfig{ProjectID: "p"}
	if half.Enabled() {
		t.Error("rustore without service token enabled")
	}
}

func TestPushHMSModeValidated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(validBase+"push:\n  hms:\n    mode: silent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err == nil {
		err = cfg.Validate()
	}
	if err == nil {
		t.Fatal("push.hms.mode: silent accepted")
	}
}
