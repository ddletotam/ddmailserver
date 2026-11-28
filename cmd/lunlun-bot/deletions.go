package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// deletions: messages with a password, to be deleted at a set time. Kept in a
// file, because a timer in memory dies with the process — a restart of the
// bot left a password in a chat for good (2026-11-28).
type deletions struct {
	path string
	mu   sync.Mutex
	list []pendingDelete
}

type pendingDelete struct {
	Chat    int64 `json:"chat"`
	Message int64 `json:"message"`
	At      int64 `json:"at"` // unix seconds
}

func loadDeletions(path string) *deletions {
	d := &deletions{path: path}
	raw, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(raw, &d.list); err != nil {
			log.Printf("deletions %s: %v", path, err)
		}
	} else if !os.IsNotExist(err) {
		log.Printf("deletions %s: %v", path, err)
	}
	return d
}

func (d *deletions) add(chat, message int64, at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.list = append(d.list, pendingDelete{Chat: chat, Message: message, At: at.Unix()})
	d.saveLocked()
}

// due takes out the deletions whose time has come.
func (d *deletions) due(now time.Time) []pendingDelete {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out, keep []pendingDelete
	for _, p := range d.list {
		if p.At <= now.Unix() {
			out = append(out, p)
		} else {
			keep = append(keep, p)
		}
	}
	if len(out) > 0 {
		d.list = keep
		d.saveLocked()
	}
	return out
}

func (d *deletions) saveLocked() {
	if d.path == "" {
		return
	}
	raw, _ := json.Marshal(d.list)
	tmp := d.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(d.path), 0o700); err != nil {
		log.Printf("deletions: %v", err)
		return
	}
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("deletions: %v", err)
		return
	}
	if err := os.Rename(tmp, d.path); err != nil {
		log.Printf("deletions: %v", err)
	}
}

// runDeletions deletes due messages every 15 seconds; overdue ones (the bot
// was down) go first thing after start. Telegram lets a bot delete its own
// message for 48 hours.
func (b *bot) runDeletions(ctx context.Context) {
	for ctx.Err() == nil {
		for _, p := range b.del.due(time.Now()) {
			if err := b.tg.deleteMessage(ctx, p.Chat, p.Message); err != nil {
				log.Printf("delete credentials message %d in %d: %v", p.Message, p.Chat, err)
			}
		}
		sleep(ctx, 15*time.Second)
	}
}
