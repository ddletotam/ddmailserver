package config

import (
	"fmt"
	"os"

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
	LDAPPort    int    `yaml:"ldap_port"`    // address book lookups (default 10389)
	WebPort     int    `yaml:"web_port"`
	WebHost     string `yaml:"web_host"`
	Domain      string `yaml:"domain"` // Mail server hostname (e.g., mail.example.com)
	Locale      string `yaml:"locale"`
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
}

type SyncConfig struct {
	Interval       int `yaml:"interval"`
	MaxConnections int `yaml:"max_connections"`
}

type WorkersConfig struct {
	CPULimit          int `yaml:"cpu_limit"`
	IMAPWorkerPercent int `yaml:"imap_worker_percent"`
	QueueSize         int `yaml:"queue_size"`
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
	if c.Workers.CPULimit < 1 || c.Workers.CPULimit > 100 {
		return fmt.Errorf("CPU limit must be between 1 and 100")
	}
	if c.Workers.IMAPWorkerPercent < 0 || c.Workers.IMAPWorkerPercent > 100 {
		return fmt.Errorf("IMAP worker percent must be between 0 and 100")
	}
	if c.Workers.QueueSize < 1 {
		return fmt.Errorf("queue size must be at least 1")
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
