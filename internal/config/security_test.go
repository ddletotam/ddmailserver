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
