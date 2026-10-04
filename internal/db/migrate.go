package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

// BaselineVersion is the last migration that production had applied by hand
// before the server started applying migrations itself. A database that has
// the schema but no schema_migrations table is adopted at this version: every
// migration up to and including it is recorded as applied without running.
//
// Never raise it: databases adopted later must still get every migration
// after 050 executed for real.
const BaselineVersion = "050"

// migrationLockKey is the pg_advisory_lock key that serialises migration runs
// of several server instances starting against one database ("ddmigr").
const migrationLockKey int64 = 0x64646d696772

// noTxMarker on a line of its own makes the runner execute the file outside a
// transaction (for CREATE INDEX CONCURRENTLY and the like). Such a file must be
// safe to re-run: a failure halfway leaves its earlier statements applied.
const noTxMarker = "-- migrate:no-transaction"

// migrationFilePattern is NNN[x]_snake_name.sql.
const migrationFilePattern = `^([0-9]{3,})([a-z]?)_([a-z0-9_]+)\.sql$`

// Migration is one SQL file.
type Migration struct {
	Version  string // "020a"
	Name     string // "calendar_event_sync"
	File     string // "020a_calendar_event_sync.sql"
	SQL      string // LF line endings
	Checksum string // sha256 of SQL, hex
	NoTx     bool

	num    int
	suffix string
}

func (m Migration) less(o Migration) bool {
	if m.num != o.num {
		return m.num < o.num
	}
	return m.suffix < o.suffix
}

// parseVersion splits "020a" into (20, "a").
func parseVersion(v string) (int, string, error) {
	i := 0
	for i < len(v) && v[i] >= '0' && v[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, "", fmt.Errorf("bad migration version %q", v)
	}
	n, err := strconv.Atoi(v[:i])
	if err != nil {
		return 0, "", fmt.Errorf("bad migration version %q: %w", v, err)
	}
	return n, v[i:], nil
}

// versionAtMost reports whether migration m is not newer than version v.
func (m Migration) versionAtMost(v string) (bool, error) {
	n, s, err := parseVersion(v)
	if err != nil {
		return false, err
	}
	if m.num != n {
		return m.num < n, nil
	}
	return m.suffix <= s, nil
}

// LoadMigrations reads *.sql files from the root of fsys and returns them in
// apply order. Names that don't follow NNN[x]_name.sql and duplicate versions
// are errors: the order must never depend on how a directory listing sorts.
func LoadMigrations(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	fileRe := regexp.MustCompile(migrationFilePattern)
	var out []Migration
	seen := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".sql" {
			continue
		}
		mm := fileRe.FindStringSubmatch(e.Name())
		if mm == nil {
			return nil, fmt.Errorf("migration %s: name must be NNN[x]_snake_name.sql", e.Name())
		}
		version := mm[1] + mm[2]
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrations %s and %s share version %s", prev, e.Name(), version)
		}
		seen[version] = e.Name()
		num, err := strconv.Atoi(mm[1])
		if err != nil {
			return nil, fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		raw, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		// Windows checkouts get CRLF; hash and run the same bytes everywhere.
		text := strings.ReplaceAll(string(raw), "\r\n", "\n")
		sum := sha256.Sum256([]byte(text))
		noTx := false
		for _, line := range strings.Split(text, "\n") {
			if strings.TrimSpace(line) == noTxMarker {
				noTx = true
				break
			}
		}
		out = append(out, Migration{
			Version:  version,
			Name:     mm[3],
			File:     e.Name(),
			SQL:      text,
			Checksum: hex.EncodeToString(sum[:]),
			NoTx:     noTx,
			num:      num,
			suffix:   mm[2],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].less(out[j]) })
	return out, nil
}

// AppliedMigration is a row of schema_migrations.
type AppliedMigration struct {
	Version  string
	Name     string
	Checksum string
	Baseline bool
}

// Database states as seen before migrating.
const (
	MigrationStateEmpty    = "empty"    // no tables: run everything
	MigrationStateBaseline = "baseline" // schema without schema_migrations: adopt at BaselineVersion
	MigrationStateTracked  = "tracked"  // schema_migrations exists
)

// MigrationPlan is what RunMigrations would do.
type MigrationPlan struct {
	State    string
	Applied  []AppliedMigration
	Baseline []Migration // recorded as applied without running (state baseline)
	Pending  []Migration // executed, in this order
	Unknown  []string    // versions recorded in the database but absent from the binary
	Modified []string    // applied files whose checksum changed since
	Late     []string    // pending versions older than the newest applied one
}

const createSchemaMigrations = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version      TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    checksum     TEXT NOT NULL,
    applied_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    execution_ms INTEGER NOT NULL DEFAULT 0,
    baseline     BOOLEAN NOT NULL DEFAULT false
)`

// ctxQuerier is what *sql.Conn and *sql.Tx have in common.
type ctxQuerier interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

// schemaCheck is one fact the production schema at BaselineVersion has.
type schemaCheck struct {
	what  string
	query string
	args  []interface{}
}

func columnCheck(table, column, origin string) schemaCheck {
	return schemaCheck{
		what: fmt.Sprintf("column %s.%s (%s)", table, column, origin),
		// pg_attribute, not information_schema: the latter hides columns of
		// tables the current role has no privileges on.
		query: `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a
			JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
			JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public' AND c.relname = $1 AND a.attname = $2
			  AND a.attnum > 0 AND NOT a.attisdropped)`,
		args: []interface{}{table, column},
	}
}

func tableCheck(table, origin string) schemaCheck {
	return schemaCheck{
		what:  fmt.Sprintf("table %s (%s)", table, origin),
		query: `SELECT to_regclass('public.' || $1) IS NOT NULL`,
		args:  []interface{}{table},
	}
}

// baselineChecks fingerprint a database that went through every migration up
// to BaselineVersion. A database that lacks any of them is not adopted: it is
// either an older hand-migrated schema or something else entirely, and
// guessing would either skip migrations it needs or run ones it already has.
func baselineChecks() []schemaCheck {
	return []schemaCheck{
		tableCheck("users", "001"),
		tableCheck("messages", "001"),
		tableCheck("folders", "001"),
		columnCheck("users", "is_admin", "010"),
		tableCheck("address_books", "018"),
		columnCheck("attachments", "content_id", "020"),
		tableCheck("calendar_event_sync_queue", "020a"),
		columnCheck("accounts", "consecutive_errors", "026"),
		tableCheck("account_logs", "026a"),
		tableCheck("eas_devices", "026b"),
		columnCheck("accounts", "aliases", "027"),
		columnCheck("calendar_events", "soft_deleted_at", "027a"),
		tableCheck("folder_subscriptions", "029"),
		columnCheck("users", "is_banned", "030"),
		columnCheck("messages", "date_tz", "032"),
		columnCheck("messages", "raw_email", "032a"),
		tableCheck("avatar_cache", "033"),
		tableCheck("spam_check_weights", "034"),
		columnCheck("flag_sync_queue", "next_attempt_at", "037"),
		{
			what:  "index messages_user_message_id_uq (042)",
			query: `SELECT to_regclass('public.messages_user_message_id_uq') IS NOT NULL`,
		},
		tableCheck("message_changes", "043"),
		columnCheck("outbox_messages", "next_attempt_at", "044"),
		columnCheck("calendar_sources", "identity_email", "045"),
		tableCheck("calendar_event_sync_dead_letters", "046"),
		tableCheck("app_passwords", "047"),
		columnCheck("calendar_events", "component", "048"),
		{
			what: "no foreign key flag_sync_queue_message_id_fkey (050)",
			query: `SELECT NOT EXISTS (SELECT 1 FROM pg_constraint
				WHERE conname = 'flag_sync_queue_message_id_fkey')`,
		},
	}
}

// inspect works out the plan. It only reads.
func inspect(ctx context.Context, q ctxQuerier, all []Migration) (*MigrationPlan, error) {
	plan := &MigrationPlan{}

	var tracked bool
	if err := q.QueryRowContext(ctx,
		`SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&tracked); err != nil {
		return nil, fmt.Errorf("check schema_migrations: %w", err)
	}

	if !tracked {
		var tables int
		if err := q.QueryRowContext(ctx, `
			SELECT count(*) FROM pg_catalog.pg_class c
			JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p', 'v', 'm', 'f')`).Scan(&tables); err != nil {
			return nil, fmt.Errorf("count tables: %w", err)
		}
		if tables == 0 {
			plan.State = MigrationStateEmpty
			plan.Pending = all
			return plan, nil
		}

		var missing []string
		for _, c := range baselineChecks() {
			var ok bool
			if err := q.QueryRowContext(ctx, c.query, c.args...).Scan(&ok); err != nil {
				return nil, fmt.Errorf("check %s: %w", c.what, err)
			}
			if !ok {
				missing = append(missing, c.what)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("database has %d table(s) but no schema_migrations, and its schema "+
				"is not the one migrations up to %s produce, so it cannot be adopted automatically; "+
				"missing: %s. Bring it to the %s schema by hand (the files are in migrations/), "+
				"or point the server at an empty database",
				tables, BaselineVersion, strings.Join(missing, "; "), BaselineVersion)
		}

		plan.State = MigrationStateBaseline
		for _, m := range all {
			old, err := m.versionAtMost(BaselineVersion)
			if err != nil {
				return nil, err
			}
			if old {
				plan.Baseline = append(plan.Baseline, m)
			} else {
				plan.Pending = append(plan.Pending, m)
			}
		}
		return plan, nil
	}

	plan.State = MigrationStateTracked
	rows, err := q.QueryContext(ctx,
		`SELECT version, name, checksum, baseline FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	applied := make(map[string]AppliedMigration)
	for rows.Next() {
		var a AppliedMigration
		if err := rows.Scan(&a.Version, &a.Name, &a.Checksum, &a.Baseline); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[a.Version] = a
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}

	known := make(map[string]bool, len(all))
	var newest *Migration
	for i, m := range all {
		known[m.Version] = true
		a, ok := applied[m.Version]
		if !ok {
			continue
		}
		plan.Applied = append(plan.Applied, a)
		if a.Checksum != m.Checksum {
			plan.Modified = append(plan.Modified, m.File)
		}
		newest = &all[i]
	}
	for v := range applied {
		if !known[v] {
			plan.Unknown = append(plan.Unknown, v)
		}
	}
	sort.Strings(plan.Unknown)
	for _, m := range all {
		if _, ok := applied[m.Version]; ok {
			continue
		}
		plan.Pending = append(plan.Pending, m)
		if newest != nil && m.less(*newest) {
			plan.Late = append(plan.Late, m.File)
		}
	}
	return plan, nil
}

// PlanMigrations reports what RunMigrations would do, without changing
// anything (it does not take the lock, so it can race with a running migrate).
func (db *DB) PlanMigrations(ctx context.Context, fsys fs.FS) (*MigrationPlan, error) {
	all, err := LoadMigrations(fsys)
	if err != nil {
		return nil, err
	}
	return inspect(ctx, db.DB, all)
}

// RunMigrations brings the schema up to date: it creates schema_migrations if
// needed (adopting an existing production schema at BaselineVersion), then
// executes every pending migration in order, each in its own transaction
// together with its schema_migrations row. A session advisory lock keeps
// concurrently starting instances from migrating at the same time.
func (db *DB) RunMigrations(ctx context.Context, fsys fs.FS) error {
	all, err := LoadMigrations(fsys)
	if err != nil {
		return err
	}

	conn, err := db.DB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrations: get connection: %w", err)
	}
	defer conn.Close()

	var locked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, migrationLockKey).Scan(&locked); err != nil {
		return fmt.Errorf("migrations: advisory lock: %w", err)
	}
	if !locked {
		log.Printf("migrations: another instance is migrating, waiting for it")
		if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
			return fmt.Errorf("migrations: advisory lock: %w", err)
		}
	}
	defer func() {
		// Background context: the unlock must happen even if ctx is done.
		if _, err := conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockKey); err != nil {
			log.Printf("migrations: advisory unlock: %v", err)
		}
	}()

	plan, err := inspect(ctx, conn, all)
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}

	switch plan.State {
	case MigrationStateEmpty:
		log.Printf("migrations: empty database, creating the schema from scratch (%d migrations)", len(all))
		if _, err := conn.ExecContext(ctx, createSchemaMigrations); err != nil {
			return fmt.Errorf("migrations: create schema_migrations: %w", err)
		}
	case MigrationStateBaseline:
		if err := adoptBaseline(ctx, conn, plan.Baseline); err != nil {
			return fmt.Errorf("migrations: %w", err)
		}
		log.Printf("migrations: existing schema adopted at baseline %s: %d migrations recorded as applied without running them",
			BaselineVersion, len(plan.Baseline))
	}

	for _, f := range plan.Modified {
		log.Printf("migrations: WARNING: %s was changed after it had been applied; the change is NOT applied to this database", f)
	}
	if len(plan.Unknown) > 0 {
		log.Printf("migrations: WARNING: database has migrations this binary does not know: %s (running an older binary?)",
			strings.Join(plan.Unknown, ", "))
	}
	for _, f := range plan.Late {
		log.Printf("migrations: WARNING: %s is older than the newest applied migration; applying it anyway", f)
	}

	if len(plan.Pending) == 0 {
		log.Printf("migrations: schema is up to date (%d applied)", len(all))
		return nil
	}
	log.Printf("migrations: %d pending", len(plan.Pending))
	for _, m := range plan.Pending {
		start := time.Now()
		if err := applyMigration(ctx, conn, m); err != nil {
			return fmt.Errorf("migrations: %s: %w", m.File, err)
		}
		log.Printf("migrations: applied %s (%s)", m.File, time.Since(start).Round(time.Millisecond))
	}
	log.Printf("migrations: done, schema is at %s", plan.Pending[len(plan.Pending)-1].Version)
	return nil
}

// adoptBaseline creates schema_migrations and records the baseline rows in
// one transaction, so a crash leaves either no table (and the next start
// re-checks) or a complete baseline.
func adoptBaseline(ctx context.Context, conn *sql.Conn, baseline []Migration) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin baseline: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, createSchemaMigrations); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	for _, m := range baseline {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, checksum, baseline) VALUES ($1, $2, $3, true)`,
			m.Version, m.Name, m.Checksum); err != nil {
			return fmt.Errorf("record baseline %s: %w", m.File, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit baseline: %w", err)
	}
	return nil
}

func applyMigration(ctx context.Context, conn *sql.Conn, m Migration) error {
	start := time.Now()
	record := `INSERT INTO schema_migrations (version, name, checksum, execution_ms) VALUES ($1, $2, $3, $4)`

	if m.NoTx {
		if _, err := conn.ExecContext(ctx, m.SQL); err != nil {
			return describeSQLError(m.SQL, err)
		}
		if _, err := conn.ExecContext(ctx, record, m.Version, m.Name, m.Checksum,
			time.Since(start).Milliseconds()); err != nil {
			return fmt.Errorf("record: %w", err)
		}
		return nil
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// No arguments: lib/pq sends it as a simple query, which may hold many
	// statements and dollar-quoted function bodies.
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return describeSQLError(m.SQL, err)
	}
	if _, err := tx.ExecContext(ctx, record, m.Version, m.Name, m.Checksum,
		time.Since(start).Milliseconds()); err != nil {
		return fmt.Errorf("record: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// describeSQLError turns PostgreSQL's character offset into a line number.
func describeSQLError(text string, err error) error {
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) || pqErr.Position == "" {
		return err
	}
	pos, convErr := strconv.Atoi(pqErr.Position)
	if convErr != nil || pos < 1 {
		return err
	}
	runes := []rune(text)
	if pos > len(runes) {
		pos = len(runes)
	}
	line := strings.Count(string(runes[:pos-1]), "\n") + 1
	return fmt.Errorf("line %d: %w", line, err)
}

// String renders the plan for -migrate=plan.
func (p *MigrationPlan) String() string {
	var b strings.Builder
	switch p.State {
	case MigrationStateEmpty:
		fmt.Fprintf(&b, "Database is empty: the whole schema will be created.\n")
	case MigrationStateBaseline:
		fmt.Fprintf(&b, "Database has a schema but no schema_migrations: it will be adopted at baseline %s.\n", BaselineVersion)
		fmt.Fprintf(&b, "Recorded as applied WITHOUT running (%d): %s .. %s\n",
			len(p.Baseline), p.Baseline[0].File, p.Baseline[len(p.Baseline)-1].File)
	case MigrationStateTracked:
		fmt.Fprintf(&b, "Applied: %d migration(s).\n", len(p.Applied))
	}
	for _, f := range p.Modified {
		fmt.Fprintf(&b, "WARNING: %s changed after it was applied (not re-run).\n", f)
	}
	if len(p.Unknown) > 0 {
		fmt.Fprintf(&b, "WARNING: database has migrations unknown to this binary: %s\n", strings.Join(p.Unknown, ", "))
	}
	for _, f := range p.Late {
		fmt.Fprintf(&b, "WARNING: %s is older than the newest applied migration.\n", f)
	}
	if len(p.Pending) == 0 {
		fmt.Fprintf(&b, "Nothing to apply.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "To apply (%d), in this order:\n", len(p.Pending))
	for _, m := range p.Pending {
		note := ""
		if m.NoTx {
			note = "  [no transaction]"
		}
		fmt.Fprintf(&b, "  %s%s\n", m.File, note)
	}
	return b.String()
}
