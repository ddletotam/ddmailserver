package db

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/ddletotam/ddmailserver/migrations"
)

func TestLoadMigrations_OrderAndNormalisation(t *testing.T) {
	fsys := fstest.MapFS{
		"021_c.sql":  {Data: []byte("SELECT 3;\n")},
		"020a_b.sql": {Data: []byte("SELECT 2;\r\nSELECT 2;\r\n")},
		"020_a.sql":  {Data: []byte("SELECT 1;\n")},
		"100_d.sql":  {Data: []byte("-- migrate:no-transaction\nCREATE INDEX CONCURRENTLY x ON y(z);\n")},
		"README.md":  {Data: []byte("ignored")},
		"embed.go":   {Data: []byte("package migrations")},
	}
	ms, err := LoadMigrations(fsys)
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	var got []string
	for _, m := range ms {
		got = append(got, m.Version+":"+m.Name)
	}
	want := "020:a 020a:b 021:c 100:d"
	if strings.Join(got, " ") != want {
		t.Fatalf("order = %v, want %s", got, want)
	}
	if strings.Contains(ms[1].SQL, "\r") {
		t.Errorf("CRLF not normalised: %q", ms[1].SQL)
	}
	lf, err := LoadMigrations(fstest.MapFS{"020a_b.sql": {Data: []byte("SELECT 2;\nSELECT 2;\n")}})
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	if lf[0].Checksum != ms[1].Checksum {
		t.Errorf("checksum depends on line endings")
	}
	if ms[0].NoTx || !ms[3].NoTx {
		t.Errorf("NoTx = %v/%v, want false/true", ms[0].NoTx, ms[3].NoTx)
	}
}

func TestLoadMigrations_Rejects(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"duplicate version": {"020_a.sql": {}, "020_b.sql": {}},
		"no number":         {"attachments.sql": {}},
		"two digits":        {"20_a.sql": {}},
		"upper case":        {"020_Attachments.sql": {}},
		"two letters":       {"020ab_x.sql": {}},
	}
	for name, fsys := range cases {
		if _, err := LoadMigrations(fsys); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestVersionAtMost(t *testing.T) {
	ms, err := LoadMigrations(fstest.MapFS{
		"049_a.sql": {}, "050_b.sql": {}, "050a_c.sql": {}, "051_d.sql": {}, "100_e.sql": {},
	})
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	want := []bool{true, true, false, false, false}
	for i, m := range ms {
		got, err := m.versionAtMost("050")
		if err != nil {
			t.Fatalf("versionAtMost: %v", err)
		}
		if got != want[i] {
			t.Errorf("%s at most 050 = %v, want %v", m.File, got, want[i])
		}
	}
}

// The embedded set must load, and the baseline must be one of its versions.
func TestEmbeddedMigrations(t *testing.T) {
	ms, err := LoadMigrations(migrations.FS())
	if err != nil {
		t.Fatalf("LoadMigrations(embedded): %v", err)
	}
	found := false
	for _, m := range ms {
		if m.Version == BaselineVersion {
			found = true
		}
		// The runner owns the transaction; a COMMIT inside a file would end it
		// early and leave the schema_migrations row outside it.
		for _, line := range strings.Split(m.SQL, "\n") {
			l := strings.ToUpper(strings.TrimSpace(line))
			if l == "BEGIN;" || l == "COMMIT;" || l == "ROLLBACK;" || strings.HasPrefix(l, "START TRANSACTION") {
				t.Errorf("%s has its own transaction control: %q", m.File, line)
			}
		}
	}
	if !found {
		t.Fatalf("baseline version %s is not among the embedded migrations", BaselineVersion)
	}
}

func openMigrateTestDB(t *testing.T, env string) *DB {
	t.Helper()
	dsn := os.Getenv(env)
	if dsn == "" {
		t.Skipf("set %s to run this test", env)
	}
	raw, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := raw.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return &DB{DB: raw}
}

// TestRunMigrations_EmptyDatabase builds the whole schema on an EMPTY database
// (MAILSERVER_MIGRATE_EMPTY_DSN; the test changes it). Several runners start
// at once to exercise the advisory lock; a second pass must be a no-op.
func TestRunMigrations_EmptyDatabase(t *testing.T) {
	db := openMigrateTestDB(t, "MAILSERVER_MIGRATE_EMPTY_DSN")
	ctx := context.Background()

	plan, err := db.PlanMigrations(ctx, migrations.FS())
	if err != nil {
		t.Fatalf("PlanMigrations: %v", err)
	}
	if plan.State != MigrationStateEmpty {
		t.Fatalf("state = %s, want an empty database", plan.State)
	}

	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = db.RunMigrations(ctx, migrations.FS())
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("runner %d: %v", i, err)
		}
	}

	all, err := LoadMigrations(migrations.FS())
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE NOT baseline`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != len(all) {
		t.Fatalf("schema_migrations has %d rows, want %d", n, len(all))
	}

	plan, err = db.PlanMigrations(ctx, migrations.FS())
	if err != nil {
		t.Fatalf("PlanMigrations after: %v", err)
	}
	if plan.State != MigrationStateTracked || len(plan.Pending) != 0 || len(plan.Modified) != 0 {
		t.Fatalf("after migrating: state=%s pending=%d modified=%v", plan.State, len(plan.Pending), plan.Modified)
	}
	// A schema built from scratch must also pass the baseline fingerprint,
	// otherwise the fingerprint asks for something the migrations don't create.
	for _, c := range baselineChecks() {
		var ok bool
		if err := db.QueryRow(c.query, c.args...).Scan(&ok); err != nil {
			t.Fatalf("%s: %v", c.what, err)
		}
		if !ok {
			t.Errorf("fresh schema fails baseline check %s", c.what)
		}
	}
}

// TestRunMigrations_AdoptsProductionSchema runs against a COPY of the
// production schema without schema_migrations (MAILSERVER_MIGRATE_PROD_DSN;
// the test changes it): migrations up to the baseline must be recorded, not
// executed, and only later ones run.
func TestRunMigrations_AdoptsProductionSchema(t *testing.T) {
	db := openMigrateTestDB(t, "MAILSERVER_MIGRATE_PROD_DSN")
	ctx := context.Background()

	plan, err := db.PlanMigrations(ctx, migrations.FS())
	if err != nil {
		t.Fatalf("PlanMigrations: %v", err)
	}
	if plan.State != MigrationStateBaseline {
		t.Fatalf("state = %s, want baseline", plan.State)
	}
	if last := plan.Baseline[len(plan.Baseline)-1]; last.Version != BaselineVersion {
		t.Fatalf("baseline ends at %s, want %s", last.Version, BaselineVersion)
	}
	for _, m := range plan.Pending {
		if ok, _ := m.versionAtMost(BaselineVersion); ok {
			t.Fatalf("%s would be executed on the production schema", m.File)
		}
	}

	if err := db.RunMigrations(ctx, migrations.FS()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	var baseline, executed int
	if err := db.QueryRow(`SELECT count(*) FILTER (WHERE baseline), count(*) FILTER (WHERE NOT baseline)
		FROM schema_migrations`).Scan(&baseline, &executed); err != nil {
		t.Fatalf("count: %v", err)
	}
	if baseline != len(plan.Baseline) || executed != len(plan.Pending) {
		t.Fatalf("baseline=%d executed=%d, want %d/%d", baseline, executed, len(plan.Baseline), len(plan.Pending))
	}
}

// TestRunMigrations_RefusesForeignSchema: a database with tables that is not
// the production schema must be refused, untouched (MAILSERVER_MIGRATE_FOREIGN_DSN;
// the test creates a table in it).
func TestRunMigrations_RefusesForeignSchema(t *testing.T) {
	db := openMigrateTestDB(t, "MAILSERVER_MIGRATE_FOREIGN_DSN")
	ctx := context.Background()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS users (id SERIAL PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	err := db.RunMigrations(ctx, migrations.FS())
	if err == nil {
		t.Fatalf("RunMigrations adopted a foreign schema")
	}
	if !strings.Contains(err.Error(), "cannot be adopted") {
		t.Errorf("unexpected error: %v", err)
	}
	var tracked bool
	if err := db.QueryRow(`SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&tracked); err != nil {
		t.Fatalf("check: %v", err)
	}
	if tracked {
		t.Errorf("schema_migrations was created in a refused database")
	}
}
