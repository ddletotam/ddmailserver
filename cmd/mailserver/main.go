package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ddletotam/ddmailserver/internal/authlimit"
	"github.com/ddletotam/ddmailserver/internal/caldav/importer"
	"github.com/ddletotam/ddmailserver/internal/clientip"
	"github.com/ddletotam/ddmailserver/internal/config"
	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/dkimsign"
	imapclient "github.com/ddletotam/ddmailserver/internal/imap/client"
	imapserver "github.com/ddletotam/ddmailserver/internal/imap/server"
	"github.com/ddletotam/ddmailserver/internal/notify"
	"github.com/ddletotam/ddmailserver/internal/oauth"
	"github.com/ddletotam/ddmailserver/internal/parser"
	"github.com/ddletotam/ddmailserver/internal/search"
	smtpmx "github.com/ddletotam/ddmailserver/internal/smtp/mx"
	smtpserver "github.com/ddletotam/ddmailserver/internal/smtp/server"
	"github.com/ddletotam/ddmailserver/internal/web"
	"github.com/ddletotam/ddmailserver/internal/worker"
	"github.com/ddletotam/ddmailserver/migrations"
	"github.com/emersion/go-message"
)

// migrateSchema applies (or, for -migrate=plan, prints) the embedded schema
// migrations. With -migrate=off it only warns about pending ones.
func migrateSchema(database *db.DB, mode string) error {
	ctx := context.Background()
	switch mode {
	case "plan":
		plan, err := database.PlanMigrations(ctx, migrations.FS())
		if err != nil {
			return err
		}
		fmt.Print(plan.String())
		return nil
	case "off":
		plan, err := database.PlanMigrations(ctx, migrations.FS())
		if err != nil {
			log.Printf("Warning: -migrate=off and the schema state is unclear: %v", err)
			return nil
		}
		if len(plan.Pending) > 0 {
			log.Printf("Warning: -migrate=off with %d pending migration(s), first %s",
				len(plan.Pending), plan.Pending[0].File)
		}
		return nil
	default:
		return database.RunMigrations(ctx, migrations.FS())
	}
}

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
	migrateMode := flag.String("migrate", "auto",
		"Schema migrations: auto (apply pending, then serve), plan (print what would be applied and exit), "+
			"only (apply pending and exit), off (do not touch the schema)")
	flag.Parse()

	switch *migrateMode {
	case "auto", "plan", "only", "off":
	default:
		log.Fatalf("Invalid -migrate=%q: want auto, plan, only or off", *migrateMode)
	}

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

	// Which proxies' X-Forwarded-* headers to believe (default: loopback).
	clientIPResolver, err := clientip.New(cfg.Security.TrustedProxies)
	if err != nil {
		log.Fatalf("Invalid security.trusted_proxies: %v", err)
	}

	// One limiter for every protocol, so guesses spread over IMAP, SMTP,
	// DAV and the web all count against the same IP and username.
	authLimiter, err := authlimit.New(cfg.Security.AuthLimit)
	if err != nil {
		log.Fatalf("Failed to create auth limiter: %v", err)
	}

	// Connect to database
	log.Printf("Connecting to database at %s:%d", cfg.Database.Host, cfg.Database.Port)
	database, err := db.Connect(cfg.Database.GetDSN())
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	// Closed explicitly at the end of shutdown — see shutdown().
	log.Printf("Database connection established")

	if err := migrateSchema(database, *migrateMode); err != nil {
		log.Fatalf("Startup aborted: %v", err)
	}
	if *migrateMode == "plan" || *migrateMode == "only" {
		return
	}

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
	for _, key := range cfg.Workers.DeprecatedKeys() {
		log.Printf("WARNING: config key %s is obsolete and ignored — size the pool with workers.imap_workers / workers.smtp_workers", key)
	}
	workersCfg := cfg.Workers.WithDefaults()
	pool := worker.NewPool(workersCfg.IMAPWorkers, workersCfg.SMTPWorkers, workersCfg.QueueSize)
	pool.Start()

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

	// Free anything the previous run left mid-send. status='sending' is set just
	// before a send starts and cleared only when it finishes, so a restart in
	// between strands the row: the scheduler picks up 'pending' and nothing
	// else. Done before the scheduler starts, while nothing can be in flight.
	if freed, err := database.RecoverStrandedOutboxMessages(); err != nil {
		log.Printf("Failed to recover stranded outbox messages: %v", err)
	} else if freed > 0 {
		log.Printf("Recovered %d outbox message(s) stranded in 'sending' by a previous run", freed)
	}

	// Start scheduler last — all dependencies must be ready before first sync cycle.
	go scheduler.Start()

	// Network listeners, closed on shutdown before the pool is drained.
	var listeners []namedStop

	// Initialize IMAP server (plain) WITHOUT the IDLE extension, but WITH the
	// notify hub: flag changes made through this listener must still publish
	// flags_changed for the desktop WS push (the hub is not IDLE-specific).
	log.Printf("Initializing IMAP server (plain, no IDLE)...")
	imapAddr := fmt.Sprintf("%s:%d", cfg.Server.WebHost, cfg.Server.IMAPPort)
	imapSrv := imapserver.NewWithHub(database, imapAddr, notifyHub)
	imapSrv.SetAuthLimiter(authLimiter)
	if searchIndexer != nil {
		imapSrv.SetSearchIndexer(searchIndexer)
	}
	go func() {
		if err := imapSrv.Start(); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Fatalf("IMAP server error: %v", err)
		}
	}()
	listeners = append(listeners, namedStop{"IMAP server", imapSrv.Stop})

	// Initialize IMAP TLS server WITH IDLE support (only TLS gets push notifications)
	if hasTLS && cfg.Server.IMAPTLSPort > 0 {
		log.Printf("Initializing IMAP TLS server with IDLE support...")
		imapTLSAddr := fmt.Sprintf("%s:%d", cfg.Server.WebHost, cfg.Server.IMAPTLSPort)
		imapTLSSrv, err := imapserver.NewWithTLSAndHub(database, imapTLSAddr, cfg.Security.TLSCert, cfg.Security.TLSKey, notifyHub)
		if err != nil {
			log.Printf("Failed to create IMAP TLS server: %v", err)
		} else {
			imapTLSSrv.SetAuthLimiter(authLimiter)
			if searchIndexer != nil {
				imapTLSSrv.SetSearchIndexer(searchIndexer)
			}
			go func() {
				if err := imapTLSSrv.StartTLS(); err != nil {
					log.Printf("IMAP TLS server error: %v", err)
				}
			}()
			listeners = append(listeners, namedStop{"IMAP TLS server", imapTLSSrv.Stop})
		}
	}

	// Initialize SMTP server (submission - for authenticated users).
	// Plaintext AUTH is allowed only when no TLS listener exists at all:
	// with TLS configured, clients must use the implicit-TLS port.
	log.Printf("Initializing SMTP server...")
	smtpAddr := fmt.Sprintf("%s:%d", cfg.Server.WebHost, cfg.Server.SMTPPort)
	smtpSrv := smtpserver.New(database, smtpAddr, hostname, !hasTLS)
	smtpSrv.SetAuthLimiter(authLimiter)
	go func() {
		if err := smtpSrv.Start(); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Fatalf("SMTP server error: %v", err)
		}
	}()
	listeners = append(listeners, namedStop{"SMTP server", smtpSrv.Stop})

	// Initialize SMTP TLS server if configured
	if hasTLS && cfg.Server.SMTPTLSPort > 0 {
		log.Printf("Initializing SMTP TLS server...")
		smtpTLSAddr := fmt.Sprintf("%s:%d", cfg.Server.WebHost, cfg.Server.SMTPTLSPort)
		smtpTLSSrv, err := smtpserver.NewWithTLS(database, smtpTLSAddr, hostname, cfg.Security.TLSCert, cfg.Security.TLSKey)
		if err != nil {
			log.Printf("Failed to create SMTP TLS server: %v", err)
		} else {
			smtpTLSSrv.SetAuthLimiter(authLimiter)
			go func() {
				if err := smtpTLSSrv.StartTLS(); err != nil {
					log.Printf("SMTP TLS server error: %v", err)
				}
			}()
			listeners = append(listeners, namedStop{"SMTP TLS server", smtpTLSSrv.Stop})
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
		listeners = append(listeners, namedStop{"MX server", mxSrv.Stop})
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
	webSrv.SetClientIPResolver(clientIPResolver)
	webSrv.SetAuthLimiter(authLimiter)
	webSrv.SetNotifyHub(notifyHub)
	// What device profiles tell clients to connect to. Not the listen ports:
	// this deployment binds 10993/10465 behind a firewall redirect from
	// 993/465, so a profile built from the listen config would be unusable.
	webSrv.SetPublicEndpoints(cfg.PublicWithDefaults())
	// Queueing a message is not sending it: without this hook the outbox row
	// waits for the next scheduler cycle, delaying every send by up to one
	// interval.
	webSrv.SetOutboxTrigger(scheduler.TriggerOutbox)
	// Saving an account or source lifts its login pause (rejected
	// credentials); this hook tries the new ones right away.
	webSrv.SetCycleTrigger(scheduler.TriggerCycle)
	if searchIndexer != nil {
		webSrv.SetSearchIndexer(searchIndexer)
	}
	go func() {
		if err := webSrv.Start(); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Fatalf("Web server error: %v", err)
		}
	}()
	listeners = append(listeners, namedStop{"web server", webSrv.Stop})
	log.Printf("Web interface available at http://%s:%d", cfg.Server.WebHost, cfg.Server.WebPort)

	log.Println("✓ MailServer started successfully")
	log.Println("Press Ctrl+C to stop")

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 2)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	sig := <-sigChan
	log.Printf("Received %v, shutting down gracefully...", sig)

	shutdown(sigChan, scheduler, idleManager, listeners, pool, database)
}

// Shutdown budget. systemd's TimeoutStopSec (90 s by default) ends in SIGKILL;
// staying well inside it keeps the stop a clean exit and restarts fast. The
// pool gets most of it: an in-flight SMTP send should finish rather than be cut
// mid-transaction (a cut send is retried on the next start — a duplicate).
const (
	shutdownBudget  = 10 * time.Second
	poolStopTimeout = 7 * time.Second
)

// namedStop is one component to stop, with a name for the shutdown log.
type namedStop struct {
	name string
	stop func() error
}

// shutdown stops everything in dependency order and never takes longer than
// shutdownBudget.
//
// What used to hold the stop until systemd killed the process: pool.Stop
// waited for workers without a limit, and a worker in a mailbox sync could
// not return — on cancellation the sync abandoned its UID FETCH, and the
// deferred LOGOUT then waited forever for a reply queued behind the undrained
// FETCH stream (fixed in imap/client; any other ctx-blind network read can
// still hang, hence the bounded wait). The pool also closed its queues while
// the IDLE manager and TriggerOutbox could still submit — a panic, not a
// hang, but no more graceful.
func shutdown(sigChan <-chan os.Signal, scheduler *worker.Scheduler, idleManager *imapclient.IdleManager,
	listeners []namedStop, pool *worker.Pool, database *db.DB) {
	started := time.Now()

	// Last resort: whatever else hangs (a listener Close, a DB call that never
	// returns), the process still exits inside the budget instead of waiting
	// for SIGKILL. A second signal exits at once.
	watchdog := time.AfterFunc(shutdownBudget, func() {
		log.Printf("Shutdown did not finish within %v — exiting forcibly", shutdownBudget)
		os.Exit(1)
	})
	defer watchdog.Stop()
	go func() {
		if sig, ok := <-sigChan; ok {
			log.Printf("Received %v again — exiting immediately", sig)
			os.Exit(1)
		}
	}()

	// 1. No new work: stop the periodic scheduler and the IDLE watchers that
	//    trigger syncs.
	scheduler.Stop()
	idleManager.Stop()

	// 2. No new clients, in reverse order of start-up.
	for i := len(listeners) - 1; i >= 0; i-- {
		if err := listeners[i].stop(); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("Stopping %s: %v", listeners[i].name, err)
		}
	}

	// 3. Let running tasks finish within the pool's share of the budget.
	//    Submits racing with this get ErrPoolStopped.
	poolErr := pool.Stop(poolStopTimeout)
	if poolErr != nil {
		log.Printf("%v — abandoning them", poolErr)
	}

	// 4. sql.DB.Close waits for queries in progress; with abandoned workers
	//    still holding connections that could be forever, so skip it then —
	//    process exit closes the sockets anyway.
	if poolErr == nil {
		if err := database.Close(); err != nil {
			log.Printf("Closing database: %v", err)
		}
	}

	log.Printf("Shutdown complete in %v", time.Since(started).Round(time.Millisecond))
}
