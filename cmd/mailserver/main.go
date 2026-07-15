package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/emersion/go-message"
	"github.com/yourusername/mailserver/internal/caldav/importer"
	"github.com/yourusername/mailserver/internal/config"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/dkimsign"
	imapclient "github.com/yourusername/mailserver/internal/imap/client"
	imapserver "github.com/yourusername/mailserver/internal/imap/server"
	"github.com/yourusername/mailserver/internal/notify"
	"github.com/yourusername/mailserver/internal/oauth"
	"github.com/yourusername/mailserver/internal/parser"
	"github.com/yourusername/mailserver/internal/search"
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

	// One-off backfill: decode RFC 2047 encoded-words in message headers
	// that were stored before the decoder landed. Runs every startup
	// because new lenient-decoder fixes might catch more rows; the query
	// only touches rows that still contain `=?...?=` markers, so an
	// already-clean DB completes in milliseconds.
	go func() {
		n, err := database.BackfillEncodedHeaders(parser.DecodeMIMEHeader)
		if err != nil {
			log.Printf("BackfillEncodedHeaders: %v", err)
			return
		}
		if n > 0 {
			log.Printf("BackfillEncodedHeaders: decoded %d messages", n)
		}
	}()

	// One-off: backfill ATTENDEE/ORGANIZER rows on calendar events whose
	// ical_data was synced before the structured-attendee write-paths landed.
	// Cheap on every subsequent boot (idempotent, no-op when DB is current).
	if err := importer.BackfillAttendees(database); err != nil {
		log.Printf("Warning: attendee backfill failed: %v", err)
	}

	// Initialize Meilisearch if configured
	var searchIndexer *search.Indexer
	if cfg.Meilisearch.Host != "" && cfg.Meilisearch.APIKey != "" {
		log.Printf("Initializing Meilisearch at %s...", cfg.Meilisearch.Host)
		searchClient := search.New(&cfg.Meilisearch)
		searchIndexer = search.NewIndexer(searchClient, database)

		if err := searchIndexer.Initialize(); err != nil {
			log.Printf("Warning: Failed to initialize Meilisearch: %v", err)
		} else {
			log.Printf("Meilisearch initialized successfully")
			// Run full reindex in background on first start
			go func() {
				if err := searchIndexer.IndexAllMessages(); err != nil {
					log.Printf("Warning: Failed to index messages: %v", err)
				}
			}()
		}
	} else {
		log.Printf("Meilisearch not configured, search will use database")
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

	// Resolve OAuth clients (config takes precedence over DB)
	var googleOAuth *oauth.GoogleOAuth
	var microsoftOAuth *oauth.MicrosoftOAuth
	if cfg.OAuth.Google.ClientID != "" && cfg.OAuth.Google.ClientSecret != "" {
		googleOAuth = oauth.NewGoogleOAuth(&cfg.OAuth.Google)
		log.Printf("Google OAuth configured (from config)")
	} else if settings, err := database.GetGoogleOAuthSettings(); err == nil && settings.ClientID != "" && settings.ClientSecret != "" {
		googleOAuth = oauth.NewGoogleOAuth(&config.GoogleOAuthConfig{
			ClientID:     settings.ClientID,
			ClientSecret: settings.ClientSecret,
			RedirectURI:  settings.RedirectURI,
		})
		log.Printf("Google OAuth configured (from database)")
	}
	if cfg.OAuth.Microsoft.ClientID != "" && cfg.OAuth.Microsoft.ClientSecret != "" {
		microsoftOAuth = oauth.NewMicrosoftOAuth(&cfg.OAuth.Microsoft)
		log.Printf("Microsoft OAuth configured (from config)")
	} else if settings, err := database.GetMicrosoftOAuthSettings(); err == nil && settings.ClientID != "" && settings.ClientSecret != "" {
		microsoftOAuth = oauth.NewMicrosoftOAuth(&config.MicrosoftOAuthConfig{
			ClientID:     settings.ClientID,
			ClientSecret: settings.ClientSecret,
			RedirectURI:  settings.RedirectURI,
		})
		log.Printf("Microsoft OAuth configured (from database)")
	}

	// Determine hostname for SMTP
	hostname := "localhost"
	if cfg.Server.Domain != "" {
		hostname = cfg.Server.Domain
	} else {
		log.Printf("WARNING: server.domain is not set — outgoing SMTP will HELO as %q, which large providers reject", hostname)
	}

	// Check if TLS is configured
	hasTLS := cfg.Security.TLSCert != "" && cfg.Security.TLSKey != ""

	// DKIM signing of direct-delivery outgoing mail (one key per domain).
	dkimSigner := dkimsign.New(cfg.DKIM.Selector, cfg.DKIM.KeyDir)
	if dkimSigner == nil {
		log.Printf("DKIM signing disabled (dkim.selector/dkim.key_dir not configured)")
	}

	// Initialize notification hub for IMAP IDLE support
	log.Printf("Initializing notification hub...")
	notifyHub := notify.NewHub()

	// Initialize spam analyzer for IMAP sync tasks
	log.Printf("Initializing spam analyzer...")
	spamAnalyzer := parser.NewAnalyzer(nil)

	// Initialize scheduler with all dependencies wired up
	log.Printf("Initializing task scheduler...")
	scheduler := worker.NewScheduler(worker.SchedulerDeps{
		Pool:            pool,
		Database:        database,
		IntervalSeconds: cfg.Sync.Interval,
		GoogleOAuth:     googleOAuth,
		MicrosoftOAuth:  microsoftOAuth,
		NotifyHub:       notifyHub,
		Hostname:        hostname,
		Analyzer:        spamAnalyzer,
		DKIMSigner:      dkimSigner,
	})

	// Initialize IDLE manager for persistent IMAP connections
	log.Printf("Initializing IMAP IDLE manager...")
	idleManager := imapclient.NewIdleManager(database)
	idleManager.SetSyncCallback(scheduler.TriggerSyncForAccount)
	idleManager.SetOAuthClients(googleOAuth, microsoftOAuth)
	go idleManager.Start()
	defer idleManager.Stop()

	// Start scheduler last — all dependencies must be ready before first sync cycle.
	go scheduler.Start()
	defer scheduler.Stop()

	// Initialize IMAP server (plain) WITHOUT IDLE support
	log.Printf("Initializing IMAP server (plain, no IDLE)...")
	imapAddr := fmt.Sprintf("%s:%d", cfg.Server.WebHost, cfg.Server.IMAPPort)
	imapSrv := imapserver.New(database, imapAddr)
	if searchIndexer != nil {
		imapSrv.SetSearchIndexer(searchIndexer)
	}
	go func() {
		if err := imapSrv.Start(); err != nil && !errors.Is(err, net.ErrClosed) {
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
			if searchIndexer != nil {
				imapTLSSrv.SetSearchIndexer(searchIndexer)
			}
			go func() {
				if err := imapTLSSrv.StartTLS(); err != nil {
					log.Printf("IMAP TLS server error: %v", err)
				}
			}()
			defer imapTLSSrv.Stop()
		}
	}

	// Initialize SMTP server (submission - for authenticated users).
	// Plaintext AUTH is allowed only when no TLS listener exists at all:
	// with TLS configured, clients must use the implicit-TLS port.
	log.Printf("Initializing SMTP server...")
	smtpAddr := fmt.Sprintf("%s:%d", cfg.Server.WebHost, cfg.Server.SMTPPort)
	smtpSrv := smtpserver.New(database, smtpAddr, hostname, !hasTLS)
	go func() {
		if err := smtpSrv.Start(); err != nil && !errors.Is(err, net.ErrClosed) {
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
		log.Printf("Initializing MX server with IDLE notifications and calendar sync...")
		mxAddr := fmt.Sprintf("%s:%d", cfg.Server.WebHost, cfg.Server.SMTPMXPort)
		mxHostname := "localhost"
		if cfg.Server.WebHost != "" && cfg.Server.WebHost != "0.0.0.0" {
			mxHostname = cfg.Server.WebHost
		}
		// Pass scheduler's calendar sync trigger to MX server
		mxSrv := smtpmx.NewWithHubAndCalendarSync(database, mxAddr, mxHostname, notifyHub, scheduler.TriggerCalendarSyncForUser)
		go func() {
			if err := mxSrv.Start(); err != nil {
				log.Printf("MX server error: %v (may need root for port 25)", err)
			}
		}()
		defer mxSrv.Stop()
	}

	// (Removed) The inbound LDAP server face is not part of the aggregation
	// design and was a remote-crash DoS (nmcclain BER parser panics on
	// malformed packets). Contacts reach standard clients via CardDAV; LDAP is
	// used only on the inbound side (pulling a corporate GAL). See
	// docs/unified-identity-aggregation.md.

	// Initialize web server
	log.Printf("Initializing web server...")
	webSrv := web.New(database, cfg.Security.JWTSecret, cfg.Server.WebHost, cfg.Server.WebPort, cfg.Server.Locale, &cfg.OAuth)
	webSrv.SetSyncIntervalSec(cfg.Sync.Interval)
	webSrv.SetNotifyHub(notifyHub)
	if searchIndexer != nil {
		webSrv.SetSearchIndexer(searchIndexer)
	}
	go func() {
		if err := webSrv.Start(); err != nil && !errors.Is(err, net.ErrClosed) {
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
