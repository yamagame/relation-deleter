//go:build integration

package integration

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/yamagame/mysql-relation-deleter/internal/schema"
	"github.com/yamagame/mysql-relation-deleter/internal/testutil/mysqltest"
)

// droppedFK is the NO ACTION constraint shipments.order_id -> orders.id
// (Implementation Notes 4.1). Without it in the schema file, shipments and
// shipment_events are not collected, so deleting orders 101/102 hits the
// still-present constraint in the database (error 1451).
const droppedFK = "fk_shipments_order"

// withoutForeignKey copies the schema file at src into dir with the named
// foreign key removed from foreign_keys, and checks that the copy still
// loads.
func withoutForeignKey(t *testing.T, src, dir, name string) string {
	t.Helper()
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}
	var fks []map[string]json.RawMessage
	if err := json.Unmarshal(doc["foreign_keys"], &fks); err != nil {
		t.Fatalf("parse foreign_keys: %v", err)
	}
	kept := slices.DeleteFunc(slices.Clone(fks), func(fk map[string]json.RawMessage) bool {
		var n string
		_ = json.Unmarshal(fk["name"], &n)
		return n == name
	})
	if len(kept) != len(fks)-1 {
		t.Fatalf("foreign key %s found %d times, want once", name, len(fks)-len(kept))
	}
	if doc["foreign_keys"], err = json.Marshal(kept); err != nil {
		t.Fatal(err)
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "schema-without-"+name+".json")
	if err := os.WriteFile(dst, append(out, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := schema.Load(dst); err != nil {
		t.Fatalf("modified schema does not load: %v", err)
	}
	return dst
}

// deleteStmtRE matches a DELETE line of the plan's Statements section.
var deleteStmtRE = regexp.MustCompile("^DELETE FROM `([^`]+)`")

// statementTables returns the table of each DELETE in the plan's
// "Statements (execution order):" section, in order (with repeats).
func statementTables(t *testing.T, out string) []string {
	t.Helper()
	const head = "Statements (execution order):\n"
	i := strings.Index(out, head)
	if i < 0 {
		t.Fatalf("plan has no Statements section:\n%s", out)
	}
	var tables []string
	for _, line := range strings.Split(out[i+len(head):], "\n") {
		if line == "" {
			break
		}
		if m := deleteStmtRE.FindStringSubmatch(line); m != nil {
			tables = append(tables, m[1])
		}
	}
	return tables
}

// TestDeleteRollbackOnFailure makes the DELETE of orders fail on an FK the
// schema file does not know about, after other tables' DELETEs succeeded in
// the same transaction, and checks that every row is restored (6.7), the
// failing table is reported and the exit code is 1.
func TestDeleteRollbackOnFailure(t *testing.T) {
	mysqltest.ForEach(t, func(t *testing.T, shared mysqltest.Target, _ *sql.DB) {
		target := mysqltest.IsolatedDB(t, shared)
		schemaPath := withoutForeignKey(t, dumpSchema(t, target), t.TempDir(), droppedFK)

		// The dry-run plan shows which tables are deleted before orders;
		// those DELETEs succeed inside the transaction and must be rolled
		// back too.
		dry := runDeleter(t, target, schemaPath, "users", "1")
		if dry.code != 0 {
			t.Fatalf("dry-run exit code = %d, want 0; stderr:\n%s", dry.code, dry.err)
		}
		stmts := statementTables(t, dry.out)
		for _, skipped := range []string{"shipments", "shipment_events"} {
			if slices.Contains(stmts, skipped) {
				t.Fatalf("%s is still in the plan without %s:\n%s", skipped, droppedFK, dry.out)
			}
		}
		at := slices.Index(stmts, "orders")
		if at < 0 {
			t.Fatalf("plan has no DELETE for orders:\n%s", dry.out)
		}
		var earlier []string
		for _, table := range stmts[:at] {
			if !slices.Contains(earlier, table) {
				earlier = append(earlier, table)
			}
		}
		for _, table := range []string{"order_items", "audit_logs"} {
			if !slices.Contains(earlier, table) {
				t.Fatalf("%s is not deleted before orders; statements in order: %v", table, stmts)
			}
		}
		t.Logf("deleted before orders (then rolled back): %v", earlier)

		before := takeSnapshot(t, target)
		for _, table := range earlier {
			if len(expectedClosure[table]) == 0 || len(before[table].rows) == 0 {
				t.Fatalf("%s has no rows to delete, so its rollback would prove nothing", table)
			}
		}

		res := runDeleter(t, target, schemaPath, "users", "1", "--execute", "--yes")
		if res.code != 1 {
			t.Errorf("exit code = %d, want 1; stdout:\n%s\nstderr:\n%s", res.code, res.out, res.err)
		}
		if !strings.Contains(res.err, "failed at table orders") {
			t.Errorf("stderr does not name the failing table orders:\n%s", res.err)
		}
		if !strings.Contains(res.err, "1451") && !strings.Contains(res.err, droppedFK) {
			t.Errorf("stderr shows neither error 1451 nor %s:\n%s", droppedFK, res.err)
		}
		if !strings.Contains(res.err, "all changes rolled back") {
			t.Errorf("stderr does not say the changes were rolled back:\n%s", res.err)
		}
		if strings.Contains(res.out, "Deleted rows:") {
			t.Errorf("a failed run reported deletions:\n%s", res.out)
		}

		// Every row is back, including those of the tables deleted before
		// orders.
		after := takeSnapshot(t, target)
		assertUnchanged(t, before, after)
		for _, table := range earlier {
			if !slices.EqualFunc(before[table].rows, after[table].rows, func(a, b snapRow) bool { return a.canon == b.canon }) {
				t.Errorf("%s: rows differ after the rollback", table)
			}
		}

		// The server is still usable and the global FK checks are untouched.
		db, err := target.Open()
		if err != nil {
			t.Fatalf("open after failure: %v", err)
		}
		defer db.Close()
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + schema.QuoteIdent("orders")).Scan(&n); err != nil {
			t.Fatalf("query after failure: %v", err)
		}
		if want := len(before["orders"].rows); n != want {
			t.Errorf("orders count = %d, want %d", n, want)
		}
		var fkChecks int
		if err := db.QueryRow("SELECT @@GLOBAL.foreign_key_checks").Scan(&fkChecks); err != nil {
			t.Fatal(err)
		}
		if fkChecks != 1 {
			t.Errorf("@@GLOBAL.foreign_key_checks = %d, want 1", fkChecks)
		}
	})
}
