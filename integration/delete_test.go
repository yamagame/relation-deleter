//go:build integration

package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yamagame/mysql-relation-deleter/internal/cli"
	"github.com/yamagame/mysql-relation-deleter/internal/schema"
	"github.com/yamagame/mysql-relation-deleter/internal/testutil/mysqltest"
)

// keyColumns names the columns that identify a row of each fixture table in
// expectedClosure: the primary key, or every column for audit_logs, which has
// no key (its identical rows are told apart by multiplicity only).
var keyColumns = map[string][]string{
	"users":                    {"id"},
	"orders":                   {"id"},
	"order_items":              {"id"},
	"shipments":                {"order_id", "seq"},
	"shipment_events":          {"id"},
	"folders":                  {"id"},
	"teams":                    {"id"},
	"team_members":             {"id"},
	"audit_logs":               {"user_id", "order_id", "action", "logged_at"},
	"newsletter_subscriptions": {"id"},
	"subscription_deliveries":  {"id"},
	"legacy_orders":            {"id"},
	"devices":                  {"device_uuid"},
	"device_tokens":            {"id"},
	"files":                    {"id"},
	"file_shares":              {"id"},
}

// expectedClosure is alice's closure (root users.id = 1) as documented in the
// header of testdata/integration/fixture.sql: exactly these rows are deleted
// by `--table users --id 1 --execute`, and every other row must survive
// unchanged. Keys are the keyColumns values joined with "|"; NULL is "NULL"
// and binary values are 0x-prefixed lowercase hex. A key listed twice means
// two identical rows. Update this table together with the fixture.
//
// 38 rows across 16 tables (tasks.md Implementation Notes 3.6).
var expectedClosure = map[string][]string{
	"users":           {"1"},                    // the root
	"orders":          {"101", "102"},           // FK chain level 1
	"order_items":     {"1001", "1002", "1003"}, // FK chain level 2
	"shipments":       {"101|1", "101|2"},       // composite PK
	"shipment_events": {"1", "2", "3"},          // composite FK to shipments
	"folders":         {"10", "11", "12", "13"}, // self reference; 13 is bob's, reached only via parent_id
	"teams":           {"1", "3"},               // 3 is bob's, reached only via lead_member_id = 11
	"team_members":    {"11", "12", "31"},       // 12 via team 1, 31 via team 3 (both bob's)
	"audit_logs": { // no primary key; rows of user 1 with order 101/102 match through both FKs
		"1|NULL|login|2024-01-01 09:00:00",
		"1|NULL|login|2024-01-01 09:00:00", // identical duplicate row
		"1|NULL|logout|2024-01-01 18:00:00",
		"1|101|order|2024-01-01 10:00:00",
		"1|102|order|2024-01-02 10:00:00",
		"2|101|support|2024-01-05 12:00:00", // bob's row, reached only via order_id
	},
	"newsletter_subscriptions": {"1", "2"}, // FK to users.email (unique, non-PK)
	"subscription_deliveries":  {"1", "2"}, // grandchild through the email FK
	"legacy_orders":            {"1", "2"}, // manual relation orders_legacy_user; 4 (NULL) survives
	"devices": { // BINARY(16) primary key
		"0x11111111111111111111111111111111", // alice-phone
		"0x00ff00ff00ff00ff00ff00ff00ff00ff", // alice-laptop
	},
	"device_tokens": {"1", "2"}, // FK to the binary PK
	"files":         {"1"},      // VARBINARY unique key referenced by file_shares
	"file_shares":   {"1"},
}

const (
	closureTotal  = 38
	closureTables = 16
)

// tableSnapshot holds every row of one table, sorted by canonical string.
type tableSnapshot struct {
	columns []string
	rows    []snapRow
}

type snapRow struct {
	canon string            // "col=val,col=val" in column order
	vals  map[string]string // column -> canonical value
}

type snapshot map[string]*tableSnapshot

// takeSnapshot reads every row of every base table in target's database.
// NULL is "NULL"; binary, varbinary and blob values are 0x hex; everything
// else is the server's text form.
func takeSnapshot(t *testing.T, target mysqltest.Target) snapshot {
	t.Helper()
	db, err := target.Open()
	if err != nil {
		t.Fatalf("open %s: %v", target.DB, err)
	}
	defer db.Close()
	snap := snapshot{}
	for _, table := range listTables(t, db, target.DB, "BASE TABLE") {
		snap[table] = snapshotTable(t, db, table)
	}
	return snap
}

func snapshotTable(t *testing.T, db *sql.DB, table string) *tableSnapshot {
	t.Helper()
	rows, err := db.Query("SELECT * FROM " + schema.QuoteIdent(table))
	if err != nil {
		t.Fatalf("select %s: %v", table, err)
	}
	defer rows.Close()
	cts, err := rows.ColumnTypes()
	if err != nil {
		t.Fatal(err)
	}
	ts := &tableSnapshot{}
	binary := make([]bool, len(cts))
	for i, ct := range cts {
		ts.columns = append(ts.columns, ct.Name())
		switch strings.ToUpper(ct.DatabaseTypeName()) {
		case "BINARY", "VARBINARY", "BLOB", "TINYBLOB", "MEDIUMBLOB", "LONGBLOB":
			binary[i] = true
		}
	}
	raw := make([]sql.RawBytes, len(cts))
	dest := make([]any, len(cts))
	for i := range raw {
		dest[i] = &raw[i]
	}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		r := snapRow{vals: map[string]string{}}
		parts := make([]string, len(cts))
		for i, b := range raw {
			var v string
			switch {
			case b == nil:
				v = "NULL"
			case binary[i]:
				v = "0x" + hex.EncodeToString(b)
			default:
				v = string(b)
			}
			r.vals[ts.columns[i]] = v
			parts[i] = ts.columns[i] + "=" + v
		}
		r.canon = strings.Join(parts, ",")
		ts.rows = append(ts.rows, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("select %s: %v", table, err)
	}
	slices.SortFunc(ts.rows, func(a, b snapRow) int { return strings.Compare(a.canon, b.canon) })
	return ts
}

// multisetMinus returns the rows of a that are not matched by a row of b,
// counting duplicates. Both inputs are sorted by canon.
func multisetMinus(a, b []snapRow) []snapRow {
	count := map[string]int{}
	for _, r := range b {
		count[r.canon]++
	}
	var out []snapRow
	for _, r := range a {
		if count[r.canon] > 0 {
			count[r.canon]--
			continue
		}
		out = append(out, r)
	}
	return out
}

func rowKey(table string, r snapRow) string {
	cols := keyColumns[table]
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = r.vals[c]
	}
	return strings.Join(parts, "|")
}

// assertDeleted checks that after is before minus exactly the rows whose
// keys are listed in want (per table, as a multiset), and that every
// surviving row is byte-identical and no row was added.
func assertDeleted(t *testing.T, before, after snapshot, want map[string][]string) {
	t.Helper()
	if !slices.Equal(sortedKeys(before), sortedKeys(after)) {
		t.Fatalf("tables changed: before %v, after %v", sortedKeys(before), sortedKeys(after))
	}
	for table := range want {
		if _, ok := before[table]; !ok {
			t.Errorf("expected deletions in unknown table %s", table)
		}
	}
	for _, table := range sortedKeys(before) {
		b, a := before[table], after[table]
		if _, ok := keyColumns[table]; !ok {
			t.Errorf("table %s has no keyColumns entry; update the expected-closure table", table)
			continue
		}
		var gotDeleted []string
		for _, r := range multisetMinus(b.rows, a.rows) {
			gotDeleted = append(gotDeleted, rowKey(table, r))
		}
		slices.Sort(gotDeleted)
		wantDeleted := slices.Clone(want[table])
		slices.Sort(wantDeleted)
		if !slices.Equal(gotDeleted, wantDeleted) {
			t.Errorf("%s: deleted rows = %q, want %q", table, gotDeleted, wantDeleted)
		}
		for _, r := range multisetMinus(a.rows, b.rows) {
			t.Errorf("%s: row added or changed by the deletion: %s", table, r.canon)
		}
	}
}

func assertUnchanged(t *testing.T, before, after snapshot) {
	t.Helper()
	assertDeleted(t, before, after, nil)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

type cliResult struct {
	code     int
	out, err string
}

// dumpSchema runs scripts/dump-schema.sh against target into a temporary
// file and returns its path (the end-to-end input of the deleter).
func dumpSchema(t *testing.T, target mysqltest.Target) string {
	t.Helper()
	requireDumpTools(t)
	out := filepath.Join(t.TempDir(), "schema.json")
	res := runDump(t, dumpEnv("MYSQL_PWD="+target.Password), append(connArgs(target, target.User), "-o", out)...)
	if res.code != 0 {
		t.Fatalf("dump-schema.sh exit code = %d, want 0; stderr:\n%s", res.code, res.stderr)
	}
	return out
}

// runDeleter runs relation-deleter in process against target with the
// dumped schema and the sample manual relations. The password is given only
// through MYSQL_PWD, and stdin is not a terminal.
func runDeleter(t *testing.T, target mysqltest.Target, schemaPath, table, id string, extra ...string) cliResult {
	t.Helper()
	return runDeleterTimeout(t, 2*time.Minute, target, schemaPath, table, id, extra...)
}

// runDeleterTimeout is runDeleter with the given deadline for the whole run.
func runDeleterTimeout(t *testing.T, timeout time.Duration, target mysqltest.Target, schemaPath, table, id string, extra ...string) cliResult {
	t.Helper()
	args := []string{
		"--schema", schemaPath,
		"--relations", repoPath(t, "testdata", "relations.sample.yaml"),
		"--table", table, "--id", id,
		"-h", target.Host, "-P", strconv.Itoa(target.Port), "-u", target.User, "-D", target.DB,
	}
	args = append(args, extra...)
	var out, errBuf bytes.Buffer
	io := cli.IO{
		In:         strings.NewReader(""),
		Out:        &out,
		Err:        &errBuf,
		IsTerminal: func() bool { return false },
		Getenv: func(k string) string {
			if k == "MYSQL_PWD" {
				return target.Password
			}
			return ""
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	code := cli.Run(ctx, args, io, cli.DefaultDeps())
	return cliResult{code: code, out: out.String(), err: errBuf.String()}
}

// deletedCounts parses the "Deleted rows:" block of report.RenderResult.
var deletedLineRE = regexp.MustCompile(`(?m)^  ([a-z_]+): ([0-9]+)$`)

func deletedCounts(t *testing.T, out string) map[string]int {
	t.Helper()
	i := strings.Index(out, "Deleted rows:\n")
	if i < 0 {
		t.Fatalf("output has no \"Deleted rows:\" block:\n%s", out)
	}
	counts := map[string]int{}
	for _, m := range deletedLineRE.FindAllStringSubmatch(out[i:], -1) {
		n, _ := strconv.Atoi(m[2])
		if _, dup := counts[m[1]]; dup {
			t.Errorf("table %s appears twice in the result", m[1])
		}
		counts[m[1]] = n
	}
	return counts
}

// planTableLineRE matches "N. <table>: <count> row(s)[ (no primary key)][ [cyclic group G]]".
var planTableLineRE = regexp.MustCompile(`(?m)^[0-9]+\. ([a-z_]+): ([0-9]+) rows?( \(no primary key\))?(?: \[cyclic group ([0-9]+)\])?$`)

// TestDeleteDryRun runs the dry-run on alice and checks the plan and that no
// row of any table changed (4.6, 5.1, 2.1, 6.6).
func TestDeleteDryRun(t *testing.T) {
	mysqltest.ForEach(t, func(t *testing.T, shared mysqltest.Target, _ *sql.DB) {
		target := mysqltest.IsolatedDB(t, shared)
		schemaPath := dumpSchema(t, target)
		before := takeSnapshot(t, target)

		res := runDeleter(t, target, schemaPath, "users", "1")
		if res.code != 0 {
			t.Fatalf("exit code = %d, want 0; stderr:\n%s", res.code, res.err)
		}
		want := fmt.Sprintf("total=%d tables=%d\n", closureTotal, closureTables)
		if !strings.HasSuffix(res.out, want) {
			t.Errorf("plan does not end with %q:\n%s", want, res.out)
		}

		// Per-table counts and cyclic groups in the plan.
		counts := map[string]int{}
		groups := map[string]string{}
		for _, m := range planTableLineRE.FindAllStringSubmatch(res.out, -1) {
			n, _ := strconv.Atoi(m[2])
			counts[m[1]] = n
			groups[m[1]] = m[4]
		}
		for _, table := range sortedKeys(expectedClosure) {
			if got, want := counts[table], len(expectedClosure[table]); got != want {
				t.Errorf("plan count for %s = %d, want %d", table, got, want)
			}
		}
		if len(counts) != closureTables {
			t.Errorf("plan lists %d tables, want %d:\n%s", len(counts), closureTables, res.out)
		}
		// 6.6: the self reference and the mutual reference are deleted with
		// FK checks off; teams and team_members share one group.
		if groups["folders"] == "" {
			t.Errorf("folders is not marked as a cyclic group:\n%s", res.out)
		}
		if groups["teams"] == "" || groups["teams"] != groups["team_members"] {
			t.Errorf("teams/team_members cyclic groups = %q/%q, want the same group", groups["teams"], groups["team_members"])
		}
		if groups["teams"] != "" && groups["teams"] == groups["folders"] {
			t.Errorf("folders and teams share cyclic group %s", groups["folders"])
		}
		for _, s := range []string{
			"via manual orders_legacy_user: legacy_orders(customer_id) -> users(id)", // 2.1
			"audit_logs: 6 rows (no primary key)",
			"SET SESSION foreign_key_checks = 0;",
			"SET SESSION foreign_key_checks = 1;",
		} {
			if !strings.Contains(res.out, s) {
				t.Errorf("plan does not contain %q:\n%s", s, res.out)
			}
		}

		assertUnchanged(t, before, takeSnapshot(t, target)) // 4.6, 5.1
	})
}

// TestDeleteExecute deletes alice with --execute --yes and checks that
// exactly her closure is deleted, every other row survives unchanged and the
// reported counts match (1.7, 2.1, 4.1, 4.3-4.5, 6.5, 6.6, 6.8). Running it
// again finds nothing and changes nothing.
func TestDeleteExecute(t *testing.T) {
	mysqltest.ForEach(t, func(t *testing.T, shared mysqltest.Target, _ *sql.DB) {
		target := mysqltest.IsolatedDB(t, shared)
		schemaPath := dumpSchema(t, target)
		before := takeSnapshot(t, target)

		res := runDeleter(t, target, schemaPath, "users", "1", "--execute", "--yes")
		if res.code != 0 {
			t.Fatalf("exit code = %d, want 0; stdout:\n%s\nstderr:\n%s", res.code, res.out, res.err)
		}
		after := takeSnapshot(t, target)
		assertDeleted(t, before, after, expectedClosure)

		// Named cases, so a failure points at the feature that broke.
		has := func(table, key string) bool {
			for _, r := range after[table].rows {
				if rowKey(table, r) == key {
					return true
				}
			}
			return false
		}
		for _, c := range []struct {
			table, key string
			survives   bool
			why        string
		}{
			{"folders", "13", false, "bob's folder reached only via parent_id"},
			{"team_members", "12", false, "bob in alice's team, reached only via team_id"},
			{"teams", "3", false, "bob's team reached only via lead_member_id"},
			{"team_members", "31", false, "bob in team 3, reached only via team 3"},
			{"audit_logs", "2|101|support|2024-01-05 12:00:00", false, "bob's log for alice's order"},
			{"audit_logs", "2|NULL|login|2024-01-02 09:00:00", true, "bob's log without an order"},
			{"audit_logs", "2|201|order|2024-01-03 10:00:00", true, "bob's log for bob's order"},
			{"legacy_orders", "1", false, "manual relation"},
			{"legacy_orders", "2", false, "manual relation"},
			{"legacy_orders", "4", true, "customer_id NULL"},
			{"devices", "0x11111111111111111111111111111111", false, "alice's binary-key device"},
			{"devices", "0x00ff00ff00ff00ff00ff00ff00ff00ff", false, "alice's binary-key device"},
			{"device_tokens", "1", false, "child of a binary key"},
			{"device_tokens", "2", false, "child of a binary key"},
			{"newsletter_subscriptions", "1", false, "FK to users.email"},
			{"newsletter_subscriptions", "2", false, "FK to users.email"},
			{"subscription_deliveries", "1", false, "grandchild through users.email"},
			{"subscription_deliveries", "2", false, "grandchild through users.email"},
			{"users", "2", true, "bob"},
		} {
			if got := has(c.table, c.key); got != c.survives {
				t.Errorf("%s %s (%s): survives = %v, want %v", c.table, c.key, c.why, got, c.survives)
			}
		}
		// audit_logs rows matched through both FKs are counted once: 8 rows
		// before, 6 deleted, 2 left.
		if n := len(after["audit_logs"].rows); n != 2 {
			t.Errorf("audit_logs rows left = %d, want 2", n)
		}

		// 6.8: the reported counts match the expected deletions per table.
		counts := deletedCounts(t, res.out)
		wantCounts := map[string]int{}
		for table, keys := range expectedClosure {
			wantCounts[table] = len(keys)
		}
		if !maps.Equal(counts, wantCounts) {
			t.Errorf("deleted counts = %v, want %v\n%s", counts, wantCounts, res.out)
		}
		want := fmt.Sprintf("total=%d tables=%d\n", closureTotal, closureTables)
		if !strings.HasSuffix(res.out, want) {
			t.Errorf("result does not end with %q:\n%s", want, res.out)
		}
		if !strings.Contains(res.out, "Rows to delete:\n") {
			t.Errorf("no summary before deleting:\n%s", res.out)
		}

		// Idempotence: the root is gone, so a second run finds nothing
		// (exit 2) and changes nothing.
		again := runDeleter(t, target, schemaPath, "users", "1", "--execute", "--yes")
		if again.code != 2 {
			t.Errorf("second run exit code = %d, want 2; stdout:\n%s\nstderr:\n%s", again.code, again.out, again.err)
		}
		if !strings.Contains(again.err, "no matching records in users") {
			t.Errorf("second run stderr = %q, want \"no matching records in users\"", again.err)
		}
		if strings.Contains(again.out, "Deleted rows:") {
			t.Errorf("second run reported deletions:\n%s", again.out)
		}
		assertUnchanged(t, after, takeSnapshot(t, target))
	})
}

// TestDeleteExecuteBinaryRoot deletes one device by its BINARY(16) primary
// key given as 0x hex (5.1 follow-up) and checks that only that device and
// its token are deleted.
func TestDeleteExecuteBinaryRoot(t *testing.T) {
	mysqltest.ForEach(t, func(t *testing.T, shared mysqltest.Target, _ *sql.DB) {
		target := mysqltest.IsolatedDB(t, shared)
		schemaPath := dumpSchema(t, target)
		before := takeSnapshot(t, target)

		const laptop = "0x00ff00ff00ff00ff00ff00ff00ff00ff" // alice-laptop, with device_tokens 2
		res := runDeleter(t, target, schemaPath, "devices", laptop, "--execute", "--yes")
		if res.code != 0 {
			t.Fatalf("exit code = %d, want 0; stdout:\n%s\nstderr:\n%s", res.code, res.out, res.err)
		}
		assertDeleted(t, before, takeSnapshot(t, target), map[string][]string{
			"devices":       {laptop},
			"device_tokens": {"2"},
		})
		if got, want := deletedCounts(t, res.out), map[string]int{"devices": 1, "device_tokens": 1}; !maps.Equal(got, want) {
			t.Errorf("deleted counts = %v, want %v", got, want)
		}
		if !strings.HasSuffix(res.out, "total=2 tables=2\n") {
			t.Errorf("result does not end with total=2 tables=2:\n%s", res.out)
		}
	})
}
