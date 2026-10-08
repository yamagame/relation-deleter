//go:build integration

package execute

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/yamagame/mysql-relation-deleter/internal/plan"
	"github.com/yamagame/mysql-relation-deleter/internal/testutil/mysqltest"
)

// The fixture's mutual pair teams <-> team_members uses ON DELETE SET NULL in
// both directions, so deleting it in either order never violates an FK. To
// prove that a cyclic group really runs with the checks off, the plans also
// delete bob's order 201 before its children: shipments.order_id references
// orders with NO ACTION, so that order fails while the checks are on.

// mutualPairPlan deletes team 2 <-> member 21 (they reference each other).
func mutualPairPlan(cyclic bool) *plan.Plan {
	return &plan.Plan{Groups: []plan.Group{{Cyclic: cyclic, Steps: []plan.Step{
		{Table: "teams", Expected: 1, Statements: []plan.Statement{{SQL: "DELETE FROM `teams` WHERE `id` IN (?)", Args: []any{2}}}},
		{Table: "team_members", Expected: 1, Statements: []plan.Statement{{SQL: "DELETE FROM `team_members` WHERE `id` IN (?)", Args: []any{21}}}},
	}}}, Total: 2, TableCount: 2}
}

// parentFirstPlan deletes order 201 before the rows that reference it, in an
// order the FK checks reject.
func parentFirstPlan(cyclic bool) *plan.Plan {
	return &plan.Plan{Groups: []plan.Group{{Cyclic: cyclic, Steps: []plan.Step{
		{Table: "orders", Expected: 1, Statements: []plan.Statement{{SQL: "DELETE FROM `orders` WHERE `id` IN (?)", Args: []any{201}}}},
		{Table: "shipments", Expected: 1, Statements: []plan.Statement{{SQL: "DELETE FROM `shipments` WHERE (`order_id`, `seq`) IN ((?, ?))", Args: []any{201, 1}}}},
		{Table: "shipment_events", Expected: 1, Statements: []plan.Statement{{SQL: "DELETE FROM `shipment_events` WHERE `id` IN (?)", Args: []any{4}}}},
		{Table: "order_items", Expected: 1, Statements: []plan.Statement{{SQL: "DELETE FROM `order_items` WHERE `id` IN (?)", Args: []any{2001}}}},
	}}}, Total: 4, TableCount: 4}
}

type probe struct {
	name  string
	query string
	args  []any
}

var probes = []probe{
	{"team 2", "SELECT COUNT(*) FROM teams WHERE id = ?", []any{2}},
	{"member 21", "SELECT COUNT(*) FROM team_members WHERE id = ?", []any{21}},
	{"order 201", "SELECT COUNT(*) FROM orders WHERE id = ?", []any{201}},
	{"shipment (201,1)", "SELECT COUNT(*) FROM shipments WHERE order_id = ? AND seq = ?", []any{201, 1}},
	{"shipment event 4", "SELECT COUNT(*) FROM shipment_events WHERE id = ?", []any{4}},
	{"order item 2001", "SELECT COUNT(*) FROM order_items WHERE id = ?", []any{2001}},
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func count(t *testing.T, q queryer, p probe) int {
	t.Helper()
	var n int
	if err := q.QueryRowContext(context.Background(), p.query, p.args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", p.name, err)
	}
	return n
}

func fkChecks(t *testing.T, q queryer) int {
	t.Helper()
	var v int
	if err := q.QueryRowContext(context.Background(), "SELECT @@SESSION.foreign_key_checks").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// openIsolated returns a fresh copy of the fixture and a transaction on it.
// The transaction is rolled back at cleanup if the test did not do so.
func openIsolated(t *testing.T, target mysqltest.Target) (*sql.DB, *sql.Tx) {
	t.Helper()
	db, err := mysqltest.IsolatedDB(t, target).Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return db, tx
}

func assertRowsExist(t *testing.T, q queryer) {
	t.Helper()
	for _, p := range probes {
		if n := count(t, q, p); n != 1 {
			t.Errorf("%s: %d rows, want 1", p.name, n)
		}
	}
}

func TestIntegrationCyclicGroupDisablesFKChecks(t *testing.T) {
	mysqltest.ForEach(t, func(t *testing.T, target mysqltest.Target, _ *sql.DB) {
		for _, tc := range []struct {
			name string
			p    *plan.Plan
		}{
			{"mutual pair", mutualPairPlan(true)},
			{"parent first", parentFirstPlan(true)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				db, tx := openIsolated(t, target)
				res, err := (&Executor{Tx: tx}).Run(context.Background(), tc.p)
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				var want []TableResult
				for _, st := range tc.p.Groups[0].Steps {
					want = append(want, TableResult{Table: st.Table, Expected: 1, Deleted: 1})
				}
				if !reflect.DeepEqual(res, want) {
					t.Errorf("results = %+v, want %+v", res, want)
				}
				if v := fkChecks(t, tx); v != 1 {
					t.Errorf("foreign_key_checks after Run = %d, want 1", v)
				}
				for _, st := range tc.p.Groups[0].Steps {
					s := st.Statements[0]
					var n int
					q := "SELECT COUNT(*) FROM `" + st.Table + "`" + s.SQL[len("DELETE FROM `"+st.Table+"`"):]
					if err := tx.QueryRow(q, s.Args...).Scan(&n); err != nil {
						t.Fatal(err)
					}
					if n != 0 {
						t.Errorf("%s: %d rows left inside the transaction", st.Table, n)
					}
				}
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
				assertRowsExist(t, db)
			})
		}
	})
}

func TestIntegrationNonCyclicGroupFailsOnFK(t *testing.T) {
	mysqltest.ForEach(t, func(t *testing.T, target mysqltest.Target, _ *sql.DB) {
		db, tx := openIsolated(t, target)
		res, err := (&Executor{Tx: tx}).Run(context.Background(), parentFirstPlan(false))
		var ee *ExecError
		if !errors.As(err, &ee) {
			t.Fatalf("err = %v, want *ExecError", err)
		}
		if ee.Table != "orders" {
			t.Errorf("ExecError.Table = %q, want orders (err: %v)", ee.Table, err)
		}
		t.Logf("expected FK error: %v", err)
		if want := []TableResult{{Table: "orders", Expected: 1}}; !reflect.DeepEqual(res, want) {
			t.Errorf("results = %+v, want %+v", res, want)
		}
		if v := fkChecks(t, tx); v != 1 {
			t.Errorf("foreign_key_checks after Run = %d, want 1", v)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		assertRowsExist(t, db)
	})
}

func TestIntegrationFailureInCyclicGroupRestoresFKChecks(t *testing.T) {
	mysqltest.ForEach(t, func(t *testing.T, target mysqltest.Target, _ *sql.DB) {
		db, tx := openIsolated(t, target)
		p := mutualPairPlan(true)
		p.Groups[0].Steps[1].Statements[0].SQL = "DELETE FROM `team_members` WHERE `no_such_column` IN (?)"
		_, err := (&Executor{Tx: tx}).Run(context.Background(), p)
		var ee *ExecError
		if !errors.As(err, &ee) || ee.Table != "team_members" {
			t.Fatalf("err = %v, want *ExecError for team_members", err)
		}
		if v := fkChecks(t, tx); v != 1 {
			t.Errorf("foreign_key_checks after failed Run = %d, want 1", v)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		assertRowsExist(t, db)
	})
}

// Cancelling the ctx given to Run (not the one given to BeginTx) partway
// through a cyclic group must still re-enable the checks on the real session.
func TestIntegrationCancelInCyclicGroupRestoresFKChecks(t *testing.T) {
	mysqltest.ForEach(t, func(t *testing.T, target mysqltest.Target, _ *sql.DB) {
		db, tx := openIsolated(t, target)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		e := &Executor{Tx: tx, Progress: func(table string, _ int64) {
			if table == "teams" {
				cancel()
			}
		}}
		res, err := e.Run(ctx, mutualPairPlan(true))
		var ee *ExecError
		if !errors.As(err, &ee) || ee.Table != "team_members" || !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want *ExecError for team_members wrapping context.Canceled", err)
		}
		if strings.Contains(err.Error(), "re-enable") {
			t.Errorf("restore failed: %v", err)
		}
		want := []TableResult{{Table: "teams", Expected: 1, Deleted: 1}, {Table: "team_members", Expected: 1}}
		if !reflect.DeepEqual(res, want) {
			t.Errorf("results = %+v, want %+v", res, want)
		}
		if v := fkChecks(t, tx); v != 1 { // fkChecks uses context.Background()
			t.Errorf("foreign_key_checks after cancelled Run = %d, want 1", v)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		assertRowsExist(t, db)
	})
}
