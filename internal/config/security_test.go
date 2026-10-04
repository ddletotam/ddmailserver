package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestSecurityConfigParsesAuthLimitAndProxies(t *testing.T) {
	src := `
security:
  trusted_proxies: ["127.0.0.1", "10.0.0.0/8"]
  auth_limit:
    max_failures_per_ip: 7
    window: 10m
    block_duration: 1h
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatal(err)
	}
	al := cfg.Security.AuthLimit
	if al.MaxFailuresPerIP != 7 || al.Window != 10*time.Minute || al.BlockDuration != time.Hour {
		t.Fatalf("auth_limit parsed wrong: %+v", al)
	}
	if len(cfg.Security.TrustedProxies) != 2 {
		t.Fatalf("trusted_proxies parsed wrong: %v", cfg.Security.TrustedProxies)
	}

	var empty Config
	if err := yaml.Unmarshal([]byte("security: {}\n"), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Security.TrustedProxies != nil {
		t.Fatal("unset trusted_proxies must stay nil (selects the loopback default)")
	}
}

func TestSecureCookiesConfig(t *testing.T) {
	// Unquoted true/false are YAML booleans; they must still land in the
	// string field.
	for src, want := range map[string]string{"true": "true", "false": "false", "auto": "auto", `"TRUE"`: "true"} {
		var cfg Config
		if err := yaml.Unmarshal([]byte("server:\n  public:\n    secure_cookies: "+src+"\n"), &cfg); err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if got := cfg.PublicWithDefaults().SecureCookies; got != want {
			t.Errorf("%s: got %q, want %q", src, got, want)
		}
	}
	var cfg Config
	if got := cfg.PublicWithDefaults().SecureCookies; got != SecureCookiesAuto {
		t.Fatalf("default: got %q, want auto", got)
	}
}

func TestValidateRejectsBadSecureCookies(t *testing.T) {
	cfg := Config{
		Server:   ServerConfig{IMAPPort: 1, SMTPPort: 1, WebPort: 1, Public: PublicEndpoints{SecureCookies: "yes"}},
		Database: DatabaseConfig{Host: "h", DBName: "d"},
		Security: SecurityConfig{JWTSecret: "s", EncryptionKey: "0123456789abcdef0123456789abcdef"},
		Workers:  WorkersConfig{CPULimit: 1, QueueSize: 1},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("secure_cookies: yes accepted")
	}
	cfg.Server.Public.SecureCookies = "auto"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}
