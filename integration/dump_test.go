//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yamagame/mysql-relation-deleter/internal/schema"
	"github.com/yamagame/mysql-relation-deleter/internal/testutil/mysqltest"
)

// dumpSummary is the stderr summary for the fixture database (1.4):
// 16 base tables (the view user_order_totals excluded) and 18 foreign keys.
const dumpSummary = "tables=16 foreign_keys=18"

// fixtureDataValues are row values from fixture.sql. None of them may appear
// in a schema file (1.3).
var fixtureDataValues = []string{
	"alice@example.com", "bob@example.com", "alice-root", "alice-docs",
	"SKU-A", "SKU-C", "yamato", "sagawa",
}

// serverVersionRE matches the server_version member, which depends on the
// container image (5.7.44 / 8.0.46 when the golden files were reviewed).
var serverVersionRE = regexp.MustCompile(`"server_version": "[^"]*"`)

// requireDumpTools skips the test when bash or the mysql client is missing.
func requireDumpTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "mysql"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH; the dump script needs it: %v", tool, err)
		}
	}
}

// repoPath returns the absolute path of a file relative to the repository
// root (the test runs in integration/).
func repoPath(t *testing.T, elem ...string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join(append([]string{".."}, elem...)...))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// dumpEnv returns the current environment without MYSQL_PWD, plus extra.
func dumpEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "MYSQL_PWD=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

type dumpResult struct {
	code   int
	stdout string
	stderr string
}

// runDump runs scripts/dump-schema.sh with args and env and returns its exit
// code and output.
func runDump(t *testing.T, env []string, args ...string) dumpResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", append([]string{repoPath(t, "scripts", "dump-schema.sh")}, args...)...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := dumpResult{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.code = exitErr.ExitCode()
	default:
		t.Fatalf("run dump-schema.sh: %v", err)
	}
	return res
}

// connArgs returns the -h/-P/-u/-D arguments for target.
func connArgs(target mysqltest.Target, user string) []string {
	return []string{"-h", target.Host, "-P", strconv.Itoa(target.Port), "-u", user, "-D", target.DB}
}

// normalizeServerVersion replaces the server_version value with a placeholder.
func normalizeServerVersion(b []byte) []byte {
	return serverVersionRE.ReplaceAll(b, []byte(`"server_version": "<normalized>"`))
}

func goldenPath(t *testing.T, target mysqltest.Target) string {
	return repoPath(t, "testdata", "schema.golden."+target.Name+".json")
}

// TestDumpMatchesGolden checks that the dump of the fixture database matches
// the reviewed golden file byte for byte, except for server_version (1.1–1.4,
// 1.7).
func TestDumpMatchesGolden(t *testing.T) {
	requireDumpTools(t)
	mysqltest.ForEach(t, func(t *testing.T, target mysqltest.Target, _ *sql.DB) {
		out := filepath.Join(t.TempDir(), "schema.json")
		res := runDump(t, dumpEnv("MYSQL_PWD="+target.Password),
			append(connArgs(target, target.User), "-o", out)...)
		if res.code != 0 {
			t.Fatalf("exit code = %d, want 0; stderr:\n%s", res.code, res.stderr)
		}
		if got := strings.TrimSpace(res.stderr); got != dumpSummary {
			t.Errorf("stderr = %q, want %q", got, dumpSummary)
		}

		got, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(goldenPath(t, target))
		if err != nil {
			t.Fatal(err)
		}
		if !serverVersionRE.Match(got) {
			t.Fatalf("output has no server_version member:\n%s", got)
		}
		if !bytes.Contains(got, []byte(`"server_version": "`+target.Name+".")) {
			t.Errorf("server_version does not start with %q", target.Name+".")
		}
		if g, w := normalizeServerVersion(got), normalizeServerVersion(want); !bytes.Equal(g, w) {
			t.Errorf("dump differs from %s (server_version ignored)\n--- got ---\n%s\n--- want ---\n%s",
				filepath.Base(goldenPath(t, target)), g, w)
		}

		// Direct checks, independent of the golden file.
		if bytes.Contains(got, []byte("user_order_totals")) {
			t.Errorf("the view user_order_totals appears in the dump")
		}
		for _, v := range fixtureDataValues {
			if bytes.Contains(got, []byte(v)) {
				t.Errorf("row value %q appears in the dump", v)
			}
		}

		s, err := schema.Load(out)
		if err != nil {
			t.Fatalf("schema.Load: %v", err)
		}
		if s.Database != target.DB {
			t.Errorf("database = %q, want %q", s.Database, target.DB)
		}
		var names []string
		for _, tb := range s.Tables {
			names = append(names, tb.Name)
		}
		if !slices.Equal(names, fixtureTables) {
			t.Errorf("tables = %v, want %v", names, fixtureTables)
		}
		if len(s.ForeignKeys) != 18 {
			t.Errorf("foreign keys = %d, want 18", len(s.ForeignKeys))
		}
		shipments, ok := s.Table("shipments")
		if !ok {
			t.Fatal("table shipments is missing")
		}
		if want := []string{"order_id", "seq"}; !slices.Equal(shipments.PrimaryKey, want) {
			t.Errorf("shipments primary key = %v, want %v", shipments.PrimaryKey, want)
		}
		var fk *schema.ForeignKey
		for i := range s.ForeignKeys {
			if s.ForeignKeys[i].Name == "fk_shipment_events_shipment" {
				fk = &s.ForeignKeys[i]
			}
		}
		if fk == nil {
			t.Fatal("foreign key fk_shipment_events_shipment is missing")
		}
		if fk.Table != "shipment_events" || fk.ReferencedTable != "shipments" ||
			!slices.Equal(fk.Columns, []string{"order_id", "shipment_seq"}) ||
			!slices.Equal(fk.ReferencedColumns, []string{"order_id", "seq"}) {
			t.Errorf("fk_shipment_events_shipment = %+v, want shipment_events(order_id, shipment_seq) -> shipments(order_id, seq)", *fk)
		}
	})
}

// TestDumpFailureKeepsExistingFile checks that a connection failure exits 1
// and leaves an existing output file and no temporary files behind (1.5).
func TestDumpFailureKeepsExistingFile(t *testing.T) {
	requireDumpTools(t)
	mysqltest.ForEach(t, func(t *testing.T, target mysqltest.Target, _ *sql.DB) {
		outDir := t.TempDir()
		tmpDir := t.TempDir()
		out := filepath.Join(outDir, "schema.json")
		sentinel := []byte("sentinel: must survive a failed dump\n")
		if err := os.WriteFile(out, sentinel, 0o644); err != nil {
			t.Fatal(err)
		}

		bad := target
		bad.Port = 1 // nothing listens here
		// MYSQL_PWD makes the script create its temporary option file in
		// TMPDIR, so the cleanup of that file is checked too.
		res := runDump(t, dumpEnv("MYSQL_PWD="+target.Password, "TMPDIR="+tmpDir),
			append(connArgs(bad, target.User), "-o", out)...)
		if res.code != 1 {
			t.Errorf("exit code = %d, want 1; stderr:\n%s", res.code, res.stderr)
		}
		if res.stderr == "" {
			t.Errorf("no error message on stderr")
		}

		got, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("existing output file: %v", err)
		}
		if !bytes.Equal(got, sentinel) {
			t.Errorf("existing output file changed: %q", got)
		}
		assertDirEntries(t, outDir, []string{"schema.json"})
		assertDirEntries(t, tmpDir, nil)
	})
}

func assertDirEntries(t *testing.T, dir string, want []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if !slices.Equal(got, want) {
		t.Errorf("files in %s = %v, want %v", dir, got, want)
	}
}

// TestDumpUsageErrors checks exit code 2 for a missing argument and for a
// password given as an argument (9.1).
func TestDumpUsageErrors(t *testing.T) {
	requireDumpTools(t)
	mysqltest.ForEach(t, func(t *testing.T, target mysqltest.Target, _ *sql.DB) {
		out := filepath.Join(t.TempDir(), "schema.json")
		const argPassword = "ArgPw-Never-Echoed"
		cases := []struct {
			name string
			args []string
		}{
			{"missing -o", connArgs(target, target.User)},
			{"missing -D", []string{"-h", target.Host, "-P", strconv.Itoa(target.Port), "-u", target.User, "-o", out}},
			{"-p", append(connArgs(target, target.User), "-o", out, "-p")},
			{"-pVALUE", append(connArgs(target, target.User), "-o", out, "-p"+argPassword)},
			{"--password=VALUE", append(connArgs(target, target.User), "-o", out, "--password="+argPassword)},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				res := runDump(t, dumpEnv("MYSQL_PWD="+target.Password), tc.args...)
				if res.code != 2 {
					t.Errorf("exit code = %d, want 2; stderr:\n%s", res.code, res.stderr)
				}
				if strings.Contains(res.stdout+res.stderr, argPassword) {
					t.Errorf("output echoes the password argument:\n%s%s", res.stdout, res.stderr)
				}
				if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("output file was created (stat err = %v)", err)
				}
			})
		}
	})
}

// TestDumpRejectsOldServer checks that a server older than 5.7.8 is rejected
// with exit code 2 and a message naming the required version (1.6). A mysql
// shim answers SELECT VERSION() with 5.7.7, since no such server is running.
func TestDumpRejectsOldServer(t *testing.T) {
	requireDumpTools(t)
	shimDir := t.TempDir()
	shim := "#!/bin/bash\nprintf '%s\\n' '5.7.7-log'\n"
	if err := os.WriteFile(filepath.Join(shimDir, "mysql"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "schema.json")
	env := dumpEnv("MYSQL_PWD=root", "PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	res := runDump(t, env, "-h", "127.0.0.1", "-P", "3306", "-u", "root", "-D", "app", "-o", out)
	if res.code != 2 {
		t.Errorf("exit code = %d, want 2; stderr:\n%s", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "5.7.8") {
		t.Errorf("stderr does not name the required version 5.7.8:\n%s", res.stderr)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("output file was created (stat err = %v)", err)
	}
}

// dumpUserPassword is the distinctive password of the temporary MySQL user in
// TestDumpPasswordNotInArgv.
const dumpUserPassword = "S3cr3t-Dump!"

// mysqlShim is a mysql wrapper that logs its argv (one argument per line) and
// whether MYSQL_PWD is in its environment, then runs the real client.
const mysqlShim = `#!/bin/bash
{
	printf '%s\n' '--- invocation ---'
	for a in "$@"; do printf 'ARG %s\n' "$a"; done
	if [ -n "${MYSQL_PWD+x}" ]; then echo 'MYSQL_PWD=set'; else echo 'MYSQL_PWD=unset'; fi
} >>"$MRD_SHIM_LOG"
exec "$MRD_SHIM_REAL" "$@"
`

// createDumpUser creates a user that can read the target database and drops
// it when the test ends.
func createDumpUser(t *testing.T, db *sql.DB, target mysqltest.Target) string {
	t.Helper()
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	user := "mrd_dump_" + hex.EncodeToString(suffix[:])
	account := fmt.Sprintf("'%s'@'%%'", user)
	stmts := []string{
		fmt.Sprintf("CREATE USER %s IDENTIFIED BY '%s'", account, dumpUserPassword),
		fmt.Sprintf("GRANT SELECT ON %s.* TO %s", schema.QuoteIdent(target.DB), account),
	}
	t.Cleanup(func() {
		if _, err := db.Exec("DROP USER IF EXISTS " + account); err != nil {
			t.Errorf("drop user %s: %v", user, err)
		}
	})
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", strings.Replace(s, dumpUserPassword, "***", 1), err)
		}
	}
	return user
}

// TestDumpPasswordNotInArgv checks that a password given through MYSQL_PWD
// never reaches a mysql process's arguments or environment, nor the script's
// output (9.1).
func TestDumpPasswordNotInArgv(t *testing.T) {
	requireDumpTools(t)
	realMySQL, err := exec.LookPath("mysql")
	if err != nil {
		t.Skipf("mysql is not on PATH: %v", err)
	}
	realMySQL, err = filepath.Abs(realMySQL)
	if err != nil {
		t.Fatal(err)
	}
	mysqltest.ForEach(t, func(t *testing.T, target mysqltest.Target, db *sql.DB) {
		user := createDumpUser(t, db, target)

		shimDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(shimDir, "mysql"), []byte(mysqlShim), 0o755); err != nil {
			t.Fatal(err)
		}
		logPath := filepath.Join(t.TempDir(), "argv.log")
		tmpDir := t.TempDir()
		out := filepath.Join(t.TempDir(), "schema.json")

		env := dumpEnv(
			"MYSQL_PWD="+dumpUserPassword,
			"PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			"MRD_SHIM_LOG="+logPath,
			"MRD_SHIM_REAL="+realMySQL,
			"TMPDIR="+tmpDir,
		)
		res := runDump(t, env, append(connArgs(target, user), "-o", out)...)
		if res.code != 0 {
			t.Fatalf("exit code = %d, want 0; stderr:\n%s", res.code, res.stderr)
		}
		if got := strings.TrimSpace(res.stderr); got != dumpSummary {
			t.Errorf("stderr = %q, want %q", got, dumpSummary)
		}
		if strings.Contains(res.stdout+res.stderr, dumpUserPassword) {
			t.Errorf("script output contains the password:\n%s%s", res.stdout, res.stderr)
		}

		logData, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("the mysql shim was not run: %v", err)
		}
		log := string(logData)
		invocations := strings.Count(log, "--- invocation ---\n")
		if invocations < 2 {
			t.Errorf("mysql invocations = %d, want at least 2 (version and schema queries)", invocations)
		}
		for _, line := range strings.Split(log, "\n") {
			if strings.Contains(line, dumpUserPassword) {
				t.Errorf("mysql argv contains the password: %q", line)
			}
		}
		if n := strings.Count(log, "MYSQL_PWD=unset\n"); n != invocations {
			t.Errorf("MYSQL_PWD reached %d of %d mysql processes:\n%s", invocations-n, invocations, log)
		}
		if !strings.Contains(log, "ARG --defaults-extra-file=") {
			t.Errorf("mysql was not given the temporary option file:\n%s", log)
		}
		assertDirEntries(t, tmpDir, nil) // the option file is removed on exit
	})
}

// TestDumpPasswordPrecedence checks that MYSQL_PWD wins over a password in
// --defaults-file (fix cd076f0), and that the defaults file is really read.
func TestDumpPasswordPrecedence(t *testing.T) {
	requireDumpTools(t)
	mysqltest.ForEach(t, func(t *testing.T, target mysqltest.Target, _ *sql.DB) {
		defaults := filepath.Join(t.TempDir(), "my.cnf")
		if err := os.WriteFile(defaults, []byte("[client]\npassword=wrong-password\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		args := slices.Clip(append(connArgs(target, target.User), "--defaults-file", defaults))

		t.Run("defaults file only", func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "schema.json")
			res := runDump(t, dumpEnv(), append(args, "-o", out)...)
			if res.code != 1 {
				t.Errorf("exit code = %d, want 1 (wrong password in the defaults file); stderr:\n%s", res.code, res.stderr)
			}
		})
		t.Run("MYSQL_PWD wins", func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "schema.json")
			res := runDump(t, dumpEnv("MYSQL_PWD="+target.Password), append(args, "-o", out)...)
			if res.code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr:\n%s", res.code, res.stderr)
			}
			if got := strings.TrimSpace(res.stderr); got != dumpSummary {
				t.Errorf("stderr = %q, want %q", got, dumpSummary)
			}
		})
	})
}
