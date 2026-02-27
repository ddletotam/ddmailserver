package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/yourusername/mailserver/internal/config"
	"github.com/yourusername/mailserver/internal/db"
	imapserver "github.com/yourusername/mailserver/internal/imap/server"
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
	// Parse command line flags
	configPath := flag.String("config", "configs/config.yaml", "Path to configuration file")
	flag.Parse()

	fmt.Println(banner)

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

	// Initialize IMAP server
	log.Printf("Initializing IMAP server...")
	imapAddr := fmt.Sprintf("%s:%d", cfg.Server.WebHost, cfg.Server.IMAPPort)
	imapSrv := imapserver.New(database, imapAddr)
	go func() {
		if err := imapSrv.Start(); err != nil {
			log.Fatalf("IMAP server error: %v", err)
		}
	}()
	defer imapSrv.Stop()

	// Initialize SMTP server
	log.Printf("Initializing SMTP server...")
	smtpAddr := fmt.Sprintf("%s:%d", cfg.Server.WebHost, cfg.Server.SMTPPort)
	smtpSrv := smtpserver.New(database, smtpAddr)
	go func() {
		if err := smtpSrv.Start(); err != nil {
			log.Fatalf("SMTP server error: %v", err)
		}
	}()
	defer smtpSrv.Stop()

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
