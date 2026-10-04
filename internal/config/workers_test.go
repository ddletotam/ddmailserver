package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// validBase is the minimum the rest of Validate demands, so the tests below
// exercise only the workers section.
const validBase = `
server:
  imap_port: 1143
  smtp_port: 1587
  web_port: 8080
database:
  host: localhost
  dbname: mailserver
security:
  jwt_secret: x
  encryption_key: "0123456789abcdef0123456789abcdef"
`

func loadString(t *testing.T, src string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// The production config still carries the old CPU-derived keys. It must keep
// loading and validating; the keys are reported as deprecated and the explicit
// defaults apply.
func TestWorkersLegacyKeysStillLoad(t *testing.T) {
	cfg := loadString(t, validBase+`
workers:
  cpu_limit: 50
  imap_worker_percent: 50
  queue_size: 1000
`)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("legacy workers section rejected: %v", err)
	}
	w := cfg.Workers.WithDefaults()
	if w.IMAPWorkers != DefaultIMAPWorkers || w.SMTPWorkers != DefaultSMTPWorkers || w.QueueSize != 1000 {
		t.Fatalf("legacy config resolved to %+v", w)
	}
	want := []string{"workers.cpu_limit", "workers.imap_worker_percent"}
	if got := cfg.Workers.DeprecatedKeys(); !reflect.DeepEqual(got, want) {
		t.Fatalf("DeprecatedKeys = %v, want %v", got, want)
	}
}

// Legacy values that the old Validate rejected (cpu_limit 0 or >100) must not
// block startup any more — they are ignored.
func TestWorkersLegacyOutOfRangeIgnored(t *testing.T) {
	cfg := loadString(t, validBase+`
workers:
  cpu_limit: 500
  imap_worker_percent: -3
`)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("ignored legacy key failed validation: %v", err)
	}
}

func TestWorkersExplicitCounts(t *testing.T) {
	cfg := loadString(t, validBase+`
workers:
  imap_workers: 6
  smtp_workers: 3
`)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	w := cfg.Workers.WithDefaults()
	if w.IMAPWorkers != 6 || w.SMTPWorkers != 3 || w.QueueSize != DefaultQueueSize {
		t.Fatalf("resolved to %+v", w)
	}
	if keys := cfg.Workers.DeprecatedKeys(); len(keys) != 0 {
		t.Fatalf("unexpected deprecated keys %v", keys)
	}
}

func TestWorkersNoSectionUsesDefaults(t *testing.T) {
	cfg := loadString(t, validBase)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config without workers section rejected: %v", err)
	}
	w := cfg.Workers.WithDefaults()
	if w.IMAPWorkers != DefaultIMAPWorkers || w.SMTPWorkers != DefaultSMTPWorkers || w.QueueSize != DefaultQueueSize {
		t.Fatalf("resolved to %+v", w)
	}
}

func TestWorkersInvalidCounts(t *testing.T) {
	for _, src := range []string{
		"workers:\n  imap_workers: -1\n",
		"workers:\n  smtp_workers: 1000\n",
		"workers:\n  queue_size: -5\n",
	} {
		cfg := loadString(t, validBase+src)
		if err := cfg.Validate(); err == nil {
			t.Errorf("accepted invalid workers config:\n%s", src)
		}
	}
}
