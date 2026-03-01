package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/emersion/go-message"
	"github.com/yourusername/mailserver/internal/config"
	"github.com/yourusername/mailserver/internal/db"
	imapclient "github.com/yourusername/mailserver/internal/imap/client"
	imapserver "github.com/yourusername/mailserver/internal/imap/server"
	"github.com/yourusername/mailserver/internal/notify"
	smtpmx "github.com/yourusername/mailserver/internal/smtp/mx"
	smtpserver "github.com/yourusername/mailserver/internal/smtp/server"
	"github.com/yourusername/mailserver/internal/web"
	"github.com/yourusername/mailserver/internal/worker"
)

const banner = `
╔══════════════════════════════════════════╗
║     MailServer - Email Aggregator        ║
║     Self-hosted IMAP/SMTP Proxy          ║
╚══════════════════════════════════════════╝
`

func main() {
	// Register charset reader for non-UTF8 email encodings
	message.CharsetReader = imapclient.CharsetReader

	// Parse command line flags
	configPath := flag.String("config", "configs/config.yaml", "Path to configuration file")
	flag.Parse()

	fmt.Print(banner)

	// Load configuration
	log.Printf("Loading configuration from %s", *configPath)
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}

	log.Printf("Configuration loaded successfully")

	// Connect to database
	log.Printf("Connecting to database at %s:%d", cfg.Database.Host, cfg.Database.Port)
	database, err := db.Connect(cfg.Database.GetDSN())
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer database.Close()
	log.Printf("Database connection established")

	// Set encryption key for password encryption/decryption
	database.SetEncryptionKey(cfg.Security.EncryptionKey)

	// Migrate any unencrypted passwords
	log.Printf("Checking for unencrypted passwords...")
	if err := database.MigrateUnencryptedPasswords(); err != nil {
		log.Fatalf("Failed to migrate unencrypted passwords: %v", err)
	}

	// Initialize worker pool
	log.Printf("Initializing worker pool...")
	pool := worker.NewPool(
		cfg.Workers.CPULimit,
		cfg.Workers.IMAPWorkerPercent,
		cfg.Workers.QueueSize,
	)
	pool.Start()
	defer pool.Stop()

	// Initialize scheduler
	log.Printf("Initializing task scheduler...")
	scheduler := worker.NewScheduler(pool, database, cfg.Sync.Interval)
	go scheduler.Start()
	defer scheduler.Stop()

	// Determine hostname for SMTP
	hostname := "localhost"
	if cfg.Server.Domain != "" {
		hostname = cfg.Server.Domain
	}

	// Check if TLS is configured
	hasTLS := cfg.Security.TLSCert != "" && cfg.Security.TLSKey != ""

	// Initialize notification hub for IMAP IDLE support
	log.Printf("Initializing notification hub for IMAP IDLE...")
	notifyHub := notify.NewHub()

	// Initialize IMAP server (plain) WITHOUT IDLE support
	log.Printf("Initializing IMAP server (plain, no IDLE)...")
	imapAddr := fmt.Sprintf("%s:%d", cfg.Server.WebHost, cfg.Server.IMAPPort)
	imapSrv := imapserver.New(database, imapAddr)
	go func() {
		if err := imapSrv.Start(); err != nil {
			log.Fatalf("IMAP server error: %v", err)
		}
	}()
	defer imapSrv.Stop()

	// Initialize IMAP TLS server WITH IDLE support (only TLS gets push notifications)
	if hasTLS && cfg.Server.IMAPTLSPort > 0 {
		log.Printf("Initializing IMAP TLS server with IDLE support...")
		imapTLSAddr := fmt.Sprintf("%s:%d", cfg.Server.WebHost, cfg.Server.IMAPTLSPort)
		imapTLSSrv, err := imapserver.NewWithTLSAndHub(database, imapTLSAddr, cfg.Security.TLSCert, cfg.Security.TLSKey, notifyHub)
		if err != nil {
			log.Printf("Failed to create IMAP TLS server: %v", err)
		} else {
			go func() {
				if err := imapTLSSrv.StartTLS(); err != nil {
					log.Printf("IMAP TLS server error: %v", err)
				}
			}()
			defer imapTLSSrv.Stop()
		}
	}

	// Initialize SMTP server (submission - for authenticated users)
	log.Printf("Initializing SMTP server...")
	smtpAddr := fmt.Sprintf("%s:%d", cfg.Server.WebHost, cfg.Server.SMTPPort)
	smtpSrv := smtpserver.New(database, smtpAddr, hostname)
	go func() {
		if err := smtpSrv.Start(); err != nil {
			log.Fatalf("SMTP server error: %v", err)
		}
	}()
	defer smtpSrv.Stop()

	// Initialize SMTP TLS server if configured
	if hasTLS && cfg.Server.SMTPTLSPort > 0 {
		log.Printf("Initializing SMTP TLS server...")
		smtpTLSAddr := fmt.Sprintf("%s:%d", cfg.Server.WebHost, cfg.Server.SMTPTLSPort)
		smtpTLSSrv, err := smtpserver.NewWithTLS(database, smtpTLSAddr, hostname, cfg.Security.TLSCert, cfg.Security.TLSKey)
		if err != nil {
			log.Printf("Failed to create SMTP TLS server: %v", err)
		} else {
			go func() {
				if err := smtpTLSSrv.StartTLS(); err != nil {
					log.Printf("SMTP TLS server error: %v", err)
				}
			}()
			defer smtpTLSSrv.Stop()
		}
	}

	// Initialize MX server (for receiving external mail) if port is configured
	if cfg.Server.SMTPMXPort > 0 {
		log.Printf("Initializing MX server with IDLE notifications...")
		mxAddr := fmt.Sprintf("%s:%d", cfg.Server.WebHost, cfg.Server.SMTPMXPort)
		mxHostname := "localhost"
		if cfg.Server.WebHost != "" && cfg.Server.WebHost != "0.0.0.0" {
			mxHostname = cfg.Server.WebHost
		}
		mxSrv := smtpmx.NewWithHub(database, mxAddr, mxHostname, notifyHub)
		go func() {
			if err := mxSrv.Start(); err != nil {
				log.Printf("MX server error: %v (may need root for port 25)", err)
			}
		}()
		defer mxSrv.Stop()
	}

	// Initialize web server
	log.Printf("Initializing web server...")
	webSrv := web.New(database, cfg.Security.JWTSecret, cfg.Server.WebHost, cfg.Server.WebPort, cfg.Server.Locale)
	go func() {
		if err := webSrv.Start(); err != nil {
			log.Fatalf("Web server error: %v", err)
		}
	}()
	defer webSrv.Stop()
	log.Printf("Web interface available at http://%s:%d", cfg.Server.WebHost, cfg.Server.WebPort)

	log.Println("✓ MailServer started successfully")
	log.Println("Press Ctrl+C to stop")

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	log.Println("\nShutting down gracefully...")

	// Cleanup will happen via defer statements
	log.Println("Shutdown complete")
}
