// lunlun-bot — Telegram bot that signs people up for lunlun.email
// (docs/lunlun-bot-plan.md). It runs outside Russia, where api.telegram.org is
// reachable; every connection goes out from here: Telegram long polling and the
// mail server's bot API (/api/bot/v1, Bearer secret). No ports to open.
//
//	lunlun-bot -token-file ~/.config/lunlun-bot/token -secret-file ~/.config/lunlun-bot/secret
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type config struct {
	tokenFile, secretFile string
	apiURL, siteURL       string
	mailHost              string
	credsTTL              time.Duration
	// allow: Telegram ids the bot talks to; empty — everyone. Until the
	// free-tier limits are live (plan, stage 2) only the owner tests sign-up.
	allow map[int64]bool
}

func main() {
	var c config
	flag.StringVar(&c.tokenFile, "token-file", "", "file with the Telegram bot token")
	flag.StringVar(&c.secretFile, "secret-file", "", "file with the mail server bot API secret")
	flag.StringVar(&c.apiURL, "api", "https://mail.lunlun.email", "mail server base URL")
	flag.StringVar(&c.siteURL, "site", "https://lunlun.email", "service site URL")
	flag.StringVar(&c.mailHost, "mail-host", "mail.lunlun.email", "IMAP/SMTP host shown to users")
	flag.DurationVar(&c.credsTTL, "creds-ttl", 10*time.Minute, "how long a message with a password stays in the chat")
	allowList := flag.String("allow", "", "comma-separated Telegram user ids the bot answers (empty: everyone)")
	flag.Parse()
	c.allow = map[int64]bool{}
	for _, f := range strings.Split(*allowList, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		id, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			log.Fatalf("-allow: %q is not a Telegram id", f)
		}
		c.allow[id] = true
	}

	token, err := readSecret(c.tokenFile, "token-file")
	if err != nil {
		log.Fatal(err)
	}
	secret, err := readSecret(c.secretFile, "secret-file")
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	b := newBot(c, newTelegram(token), newMailAPI(c.apiURL, secret))
	if err := b.tg.setCommands(ctx, botCommands); err != nil {
		log.Printf("setMyCommands: %v", err)
	}
	log.Printf("lunlun-bot started, api %s, allow-list %d ids", c.apiURL, len(c.allow))
	b.run(ctx)
	log.Printf("lunlun-bot stopped")
}

func readSecret(path, flagName string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("-%s is required", flagName)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return s, nil
}
