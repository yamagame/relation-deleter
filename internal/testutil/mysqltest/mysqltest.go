//go:build integration

// Package mysqltest provides helpers for integration tests that run against the
// Docker MySQL servers defined in testdata/integration/docker-compose.yml.
//
// Every integration test runs once per target (MySQL 5.7 and 8.0). Set
// MRD_MYSQL_VERSIONS to a comma-separated list such as "8.0" to limit the
// targets. A target whose server cannot be reached is skipped, not failed.
//
// The shared database "app" (loaded by the Docker entrypoint) is read-only for
// tests: go test runs packages in parallel, so a test that changes data must
// work on its own copy created by IsolatedDB.
package mysqltest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// VersionsEnv is the environment variable that limits the targets.
const VersionsEnv = "MRD_MYSQL_VERSIONS"

// SharedDB is the database the containers load at startup. Tests must not
// change it.
const SharedDB = "app"

// Target is one MySQL server used by the integration tests.
type Target struct {
	Name     string // "5.7" or "8.0"
	Host     string
	Port     int
	User     string
	Password string
	DB       string
}

// allTargets matches testdata/integration/docker-compose.yml. The credentials
// are for the throwaway test containers only.
var allTargets = []Target{
	{Name: "5.7", Host: "127.0.0.1", Port: 33057, User: "root", Password: "root", DB: SharedDB},
	{Name: "8.0", Host: "127.0.0.1", Port: 33080, User: "root", Password: "root", DB: SharedDB},
}

// Targets returns the targets selected by MRD_MYSQL_VERSIONS (all of them when
// it is unset or empty). It returns an error for an unknown version name.
func Targets() ([]Target, error) {
	spec := strings.TrimSpace(os.Getenv(VersionsEnv))
	if spec == "" {
		return append([]Target(nil), allTargets...), nil
	}
	var selected []Target
	for _, name := range strings.Split(spec, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		found := false
		for _, tg := range allTargets {
			if tg.Name == name {
				selected = append(selected, tg)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%s: unknown version %q (known: 5.7, 8.0)", VersionsEnv, name)
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("%s=%q selects no target", VersionsEnv, spec)
	}
	return selected, nil
}

// Addr returns "host:port".
func (tg Target) Addr() string { return fmt.Sprintf("%s:%d", tg.Host, tg.Port) }

// Config returns a driver configuration for the target's database.
func (tg Target) Config() *mysql.Config {
	cfg := mysql.NewConfig()
	cfg.User = tg.User
	cfg.Passwd = tg.Password
	cfg.Net = "tcp"
	cfg.Addr = tg.Addr()
	cfg.DBName = tg.DB
	cfg.Timeout = 5 * time.Second
	return cfg
}

// DSN returns the data source name for the target's database.
func (tg Target) DSN() string { return tg.Config().FormatDSN() }

// Open opens and pings the target's database.
func (tg Target) Open() (*sql.DB, error) {
	return open(tg.Config())
}

func open(cfg *mysql.Config) (*sql.DB, error) {
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// ForEach runs fn as a subtest named after each selected target. A target that
// cannot be connected to is skipped. The *sql.DB is closed after fn returns.
func ForEach(t *testing.T, fn func(t *testing.T, target Target, db *sql.DB)) {
	t.Helper()
	targets, err := Targets()
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range targets {
		t.Run("mysql"+tg.Name, func(t *testing.T) {
			db, err := tg.Open()
			if err != nil {
				t.Skipf("MySQL %s at %s is not reachable (start testdata/integration/docker-compose.yml): %v",
					tg.Name, tg.Addr(), err)
			}
			t.Cleanup(func() { db.Close() })
			fn(t, tg, db)
		})
	}
}

// FixturePath returns the absolute path of testdata/integration/fixture.sql.
func FixturePath() string {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")
	return filepath.Join(root, "testdata", "integration", "fixture.sql")
}

// maxIdentLen is MySQL's identifier length limit.
const maxIdentLen = 64

// IsolatedDB creates a fresh database with a unique name derived from the test
// name, loads fixture.sql into it and drops it when the test finishes. It
// returns a copy of target that points at the new database.
//
// Use it in every test that changes data, so tests in different packages
// (which go test runs in parallel) never see each other's changes.
func IsolatedDB(t testing.TB, target Target) Target {
	t.Helper()
	isolated := target
	isolated.DB = isolatedName(t.Name())
	if err := loadFixture(isolated); err != nil {
		t.Fatalf("create isolated database on MySQL %s: %v", target.Name, err)
	}
	t.Cleanup(func() {
		if err := dropDatabase(isolated); err != nil {
			t.Errorf("drop isolated database %s on MySQL %s: %v", isolated.DB, target.Name, err)
		}
	})
	return isolated
}

// isolatedName returns "app_t_<sanitized test name>_<8 random hex>", at most
// maxIdentLen bytes.
func isolatedName(testName string) string {
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		panic(err) // crypto/rand.Read does not fail on supported platforms
	}
	var b strings.Builder
	for _, r := range strings.ToLower(testName) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	const prefix = "app_t_"
	tail := "_" + hex.EncodeToString(suffix[:])
	middle := b.String()
	if limit := maxIdentLen - len(prefix) - len(tail); len(middle) > limit {
		middle = middle[:limit]
	}
	return prefix + middle + tail
}

// ResetFixture drops and recreates the target's database and reloads
// fixture.sql into it. It refuses the shared database (SharedDB), which tests
// must only read; pass a Target returned by IsolatedDB.
func ResetFixture(t testing.TB, target Target) {
	t.Helper()
	if target.DB == SharedDB {
		t.Fatalf("ResetFixture: refusing to reset the shared database %q; use IsolatedDB", SharedDB)
	}
	if err := loadFixture(target); err != nil {
		t.Fatalf("reset fixture on MySQL %s: %v", target.Name, err)
	}
}

func quoteIdent(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }

// adminConn opens a single connection with no default database and with
// multiStatements enabled (test helper only: fixture.sql is a multi-statement
// file). A single connection keeps USE in effect across statements.
func adminConn(ctx context.Context, tg Target) (*sql.Conn, func(), error) {
	cfg := tg.Config()
	cfg.DBName = ""
	cfg.MultiStatements = true
	db, err := open(cfg)
	if err != nil {
		return nil, nil, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return conn, func() { conn.Close(); db.Close() }, nil
}

// loadFixture drops and recreates tg.DB and loads fixture.sql into it.
func loadFixture(tg Target) error {
	fixture, err := os.ReadFile(FixturePath())
	if err != nil {
		return err
	}
	ctx := context.Background()
	conn, done, err := adminConn(ctx, tg)
	if err != nil {
		return err
	}
	defer done()

	name := quoteIdent(tg.DB)
	setup := fmt.Sprintf("DROP DATABASE IF EXISTS %[1]s; "+
		"CREATE DATABASE %[1]s CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci; USE %[1]s", name)
	if _, err := conn.ExecContext(ctx, setup); err != nil {
		return fmt.Errorf("recreate database %s: %w", tg.DB, err)
	}
	if _, err := conn.ExecContext(ctx, string(fixture)); err != nil {
		return fmt.Errorf("load %s into %s: %w", filepath.Base(FixturePath()), tg.DB, err)
	}
	return nil
}

func dropDatabase(tg Target) error {
	ctx := context.Background()
	conn, done, err := adminConn(ctx, tg)
	if err != nil {
		return err
	}
	defer done()
	_, err = conn.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(tg.DB))
	return err
}
