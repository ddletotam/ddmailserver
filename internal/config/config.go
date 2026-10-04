package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/authlimit"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Server      ServerConfig      `yaml:"server"`
	Database    DatabaseConfig    `yaml:"database"`
	Security    SecurityConfig    `yaml:"security"`
	Sync        SyncConfig        `yaml:"sync"`
	Workers     WorkersConfig     `yaml:"workers"`
	Logging     LoggingConfig     `yaml:"logging"`
	OAuth       OAuthConfig       `yaml:"oauth"`
	Spam        SpamConfig        `yaml:"spam"`
	Meilisearch MeilisearchConfig `yaml:"meilisearch"`
	DKIM        DKIMConfig        `yaml:"dkim"`
}

// DKIMConfig enables DKIM signing of direct-delivery outgoing mail.
// KeyDir holds one PEM RSA key per sending domain, named "<domain>.key";
// the key is looked up by the domain of the From address. Empty selector
// or key_dir disables signing.
type DKIMConfig struct {
	Selector string `yaml:"selector"`
	KeyDir   string `yaml:"key_dir"`
}

type MeilisearchConfig struct {
	Host   string `yaml:"host"` // e.g., http://127.0.0.1:7700
	APIKey string `yaml:"api_key"`
}

type SpamConfig struct {
	Enabled             bool     `yaml:"enabled"`
	SuspiciousThreshold float64  `yaml:"suspicious_threshold"`
	SpamThreshold       float64  `yaml:"spam_threshold"`
	Action              string   `yaml:"action"` // "tag", "quarantine", "reject"
	CheckHeaders        bool     `yaml:"check_headers"`
	CheckContent        bool     `yaml:"check_content"`
	CheckAttachments    bool     `yaml:"check_attachments"`
	CheckLinks          bool     `yaml:"check_links"`
	CheckSPF            bool     `yaml:"check_spf"`
	CheckDKIM           bool     `yaml:"check_dkim"`
	CheckRBL            bool     `yaml:"check_rbl"`
	DangerousExtensions []string `yaml:"dangerous_extensions"`
	MaxAttachmentSize   int64    `yaml:"max_attachment_size"`
	MaxMessageSize      int64    `yaml:"max_message_size"`
}

type OAuthConfig struct {
	Google    GoogleOAuthConfig    `yaml:"google"`
	Microsoft MicrosoftOAuthConfig `yaml:"microsoft"`
}

type GoogleOAuthConfig struct {
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	RedirectURI  string `yaml:"redirect_uri"`
}

type MicrosoftOAuthConfig struct {
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	RedirectURI  string `yaml:"redirect_uri"`
}

type ServerConfig struct {
	IMAPPort    int    `yaml:"imap_port"`
	IMAPTLSPort int    `yaml:"imap_tls_port"`
	SMTPPort    int    `yaml:"smtp_port"`
	SMTPTLSPort int    `yaml:"smtp_tls_port"`
	SMTPMXPort  int    `yaml:"smtp_mx_port"` // incoming mail (default 25)
	WebPort     int    `yaml:"web_port"`
	WebHost     string `yaml:"web_host"`
	Domain      string `yaml:"domain"` // Mail server hostname (e.g., mail.example.com)
	Locale      string `yaml:"locale"`

	// Public is what clients are told to connect to. It is NOT the same as the
	// ports above: this deployment listens on 10993/10465 and lets the firewall
	// redirect 993→10993 and 465→10465, so a device profile built from
	// IMAPTLSPort would send phones to a port nothing answers on from outside.
	Public PublicEndpoints `yaml:"public"`
}

// PublicEndpoints describes the server as reachable from the internet. Used to
// generate device configuration profiles (.mobileconfig); every field falls
// back to the conventional value via PublicWithDefaults, so an existing
// config.yaml with no `public:` section keeps working.
type PublicEndpoints struct {
	Hostname  string `yaml:"hostname"`   // defaults to Server.Domain
	IMAPPort  int    `yaml:"imap_port"`  // defaults to 993 (implicit TLS)
	SMTPPort  int    `yaml:"smtp_port"`  // defaults to 465 (implicit TLS)
	HTTPSPort int    `yaml:"https_port"` // defaults to 443, used for CalDAV/CardDAV

	// SecureCookies sets the Secure flag on session and OAuth cookies:
	// "auto" (default) marks them Secure unless the request is known to be
	// plain HTTP (see web.secureCookie); "true"/"false" force it.
	SecureCookies string `yaml:"secure_cookies"`
}

// Valid values of PublicEndpoints.SecureCookies.
const (
	SecureCookiesAuto  = "auto"
	SecureCookiesTrue  = "true"
	SecureCookiesFalse = "false"
)

// PublicWithDefaults resolves the public endpoints, filling anything the
// config left unset with the standard port for that protocol.
func (c *Config) PublicWithDefaults() PublicEndpoints {
	p := c.Server.Public
	if p.Hostname == "" {
		p.Hostname = c.Server.Domain
	}
	if p.IMAPPort == 0 {
		p.IMAPPort = 993
	}
	if p.SMTPPort == 0 {
		p.SMTPPort = 465
	}
	if p.HTTPSPort == 0 {
		p.HTTPSPort = 443
	}
	p.SecureCookies = strings.ToLower(strings.TrimSpace(p.SecureCookies))
	if p.SecureCookies == "" {
		p.SecureCookies = SecureCookiesAuto
	}
	return p
}

type DatabaseConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	DBName   string `yaml:"dbname"`
	SSLMode  string `yaml:"sslmode"`
}

type SecurityConfig struct {
	JWTSecret     string `yaml:"jwt_secret"`
	EncryptionKey string `yaml:"encryption_key"`
	TLSCert       string `yaml:"tls_cert"`
	TLSKey        string `yaml:"tls_key"`

	// TrustedProxies lists IPs/CIDRs whose X-Forwarded-For, X-Real-IP and
	// X-Forwarded-Host headers are believed. Unset means loopback only
	// (nginx on the same host); an explicit empty list trusts nobody.
	TrustedProxies []string `yaml:"trusted_proxies"`

	// AuthLimit throttles failed logins on every protocol; unset fields use
	// authlimit.DefaultConfig.
	AuthLimit authlimit.Config `yaml:"auth_limit"`
}

type SyncConfig struct {
	Interval       int `yaml:"interval"`
	MaxConnections int `yaml:"max_connections"`
}

// WorkersConfig sizes the background worker pool.
//
// Worker tasks (IMAP pulls, SMTP sends, DAV syncs) spend their time waiting on
// the network, not on the CPU, so the pool is sized by explicit counts. The old
// scheme derived them from runtime.NumCPU × cpu_limit% split by
// imap_worker_percent; on a 2-CPU host with the shipped defaults that came to
// one worker in total and zero for IMAP — mail sync never ran at all.
type WorkersConfig struct {
	IMAPWorkers int `yaml:"imap_workers"` // 0 → DefaultIMAPWorkers
	SMTPWorkers int `yaml:"smtp_workers"` // 0 → DefaultSMTPWorkers
	QueueSize   int `yaml:"queue_size"`   // 0 → DefaultQueueSize

	// Deprecated: accepted so existing config files keep loading, ignored.
	// See DeprecatedKeys.
	CPULimit          int `yaml:"cpu_limit"`
	IMAPWorkerPercent int `yaml:"imap_worker_percent"`
}

// Worker pool defaults, used when the corresponding key is absent or 0.
const (
	DefaultIMAPWorkers = 4
	DefaultSMTPWorkers = 2
	DefaultQueueSize   = 1000

	// maxWorkersPerKind bounds each kind: every IMAP worker can hold a remote
	// connection, and a typo like 400 should fail loudly, not open 400 of them.
	maxWorkersPerKind = 64
)

// WithDefaults returns the worker settings with unset (zero) values replaced
// by the defaults.
func (w WorkersConfig) WithDefaults() WorkersConfig {
	if w.IMAPWorkers == 0 {
		w.IMAPWorkers = DefaultIMAPWorkers
	}
	if w.SMTPWorkers == 0 {
		w.SMTPWorkers = DefaultSMTPWorkers
	}
	if w.QueueSize == 0 {
		w.QueueSize = DefaultQueueSize
	}
	return w
}

// DeprecatedKeys lists the obsolete worker keys present in the config, so the
// caller can warn that they no longer have any effect.
func (w WorkersConfig) DeprecatedKeys() []string {
	var keys []string
	if w.CPULimit != 0 {
		keys = append(keys, "workers.cpu_limit")
	}
	if w.IMAPWorkerPercent != 0 {
		keys = append(keys, "workers.imap_worker_percent")
	}
	return keys
}

type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Load reads configuration from a YAML file
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	return &cfg, nil
}

// Validate checks if the configuration is valid
func (c *Config) Validate() error {
	if c.Server.IMAPPort <= 0 || c.Server.IMAPPort > 65535 {
		return fmt.Errorf("invalid IMAP port: %d", c.Server.IMAPPort)
	}
	if c.Server.SMTPPort <= 0 || c.Server.SMTPPort > 65535 {
		return fmt.Errorf("invalid SMTP port: %d", c.Server.SMTPPort)
	}
	if c.Server.WebPort <= 0 || c.Server.WebPort > 65535 {
		return fmt.Errorf("invalid web port: %d", c.Server.WebPort)
	}
	if c.Database.Host == "" {
		return fmt.Errorf("database host is required")
	}
	if c.Database.DBName == "" {
		return fmt.Errorf("database name is required")
	}
	if c.Security.JWTSecret == "" {
		return fmt.Errorf("JWT secret is required")
	}
	if c.Security.EncryptionKey == "" {
		return fmt.Errorf("encryption key is required")
	}
	if len(c.Security.EncryptionKey) < 32 {
		return fmt.Errorf("encryption key must be at least 32 characters")
	}
	// cpu_limit / imap_worker_percent are deliberately not validated: they
	// are ignored, and an old value must not stop the server from starting.
	if c.Workers.IMAPWorkers < 0 || c.Workers.IMAPWorkers > maxWorkersPerKind {
		return fmt.Errorf("workers.imap_workers must be between 0 (default %d) and %d", DefaultIMAPWorkers, maxWorkersPerKind)
	}
	if c.Workers.SMTPWorkers < 0 || c.Workers.SMTPWorkers > maxWorkersPerKind {
		return fmt.Errorf("workers.smtp_workers must be between 0 (default %d) and %d", DefaultSMTPWorkers, maxWorkersPerKind)
	}
	if c.Workers.QueueSize < 0 {
		return fmt.Errorf("workers.queue_size must not be negative")
	}
	switch strings.ToLower(strings.TrimSpace(c.Server.Public.SecureCookies)) {
	case "", SecureCookiesAuto, SecureCookiesTrue, SecureCookiesFalse:
	default:
		return fmt.Errorf("server.public.secure_cookies must be auto, true or false, got %q", c.Server.Public.SecureCookies)
	}
	return nil
}

// GetDSN returns PostgreSQL connection string
func (c *DatabaseConfig) GetDSN() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		c.Host, c.Port, c.User, c.Password, c.DBName, c.SSLMode,
	)
}

// IsGoogleOAuthConfigured returns true if Google OAuth is configured
func (c *OAuthConfig) IsGoogleOAuthConfigured() bool {
	return c.Google.ClientID != "" && c.Google.ClientSecret != ""
}

// IsMicrosoftOAuthConfigured returns true if Microsoft OAuth is configured
func (c *OAuthConfig) IsMicrosoftOAuthConfigured() bool {
	return c.Microsoft.ClientID != "" && c.Microsoft.ClientSecret != ""
}

// GetSpamConfigWithDefaults returns spam config with sensible defaults
func (c *SpamConfig) GetSpamConfigWithDefaults() SpamConfig {
	cfg := *c

	// Set defaults if not configured
	if cfg.SuspiciousThreshold == 0 {
		cfg.SuspiciousThreshold = 3.0
	}
	if cfg.SpamThreshold == 0 {
		cfg.SpamThreshold = 6.0
	}
	if cfg.Action == "" {
		cfg.Action = "tag"
	}
	if len(cfg.DangerousExtensions) == 0 {
		cfg.DangerousExtensions = []string{
			".exe", ".com", ".bat", ".cmd", ".scr", ".pif",
			".js", ".jse", ".vbs", ".vbe", ".wsf", ".wsh",
			".msi", ".msp", ".dll", ".cpl", ".hta",
			".ps1", ".psm1", ".psd1",
		}
	}
	if cfg.MaxAttachmentSize == 0 {
		cfg.MaxAttachmentSize = 25 * 1024 * 1024 // 25MB
	}
	if cfg.MaxMessageSize == 0 {
		cfg.MaxMessageSize = 50 * 1024 * 1024 // 50MB
	}

	return cfg
}
