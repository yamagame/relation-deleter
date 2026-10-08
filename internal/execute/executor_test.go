package execute

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/yamagame/mysql-relation-deleter/internal/plan"
)

const (
	fkOff = "SET SESSION foreign_key_checks = 0"
	fkOn  = "SET SESSION foreign_key_checks = 1"
)

// fakeResult is the sql.Result of one fake Exec.
type fakeResult struct {
	n   int64
	err error
}

func (r fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (r fakeResult) RowsAffected() (int64, error) { return r.n, r.err }

// fakeExec records every query. Like *sql.Tx, it refuses to run a statement
// when ctx is already done (the attempt is still recorded in queries, but not
// in succeeded). A statement affects len(args) rows unless rows overrides it;
// execErr and rowsErr script failures by query text.
type fakeExec struct {
	queries   []string // attempted
	succeeded []string // executed without error
	rows      map[string]int64
	execErr   map[string]error
	rowsErr   map[string]error
	onExec    func(query string) // called after recording, before answering
}

func (f *fakeExec) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	f.queries = append(f.queries, query)
	if f.onExec != nil {
		f.onExec(query)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := f.execErr[query]; err != nil {
		return nil, err
	}
	f.succeeded = append(f.succeeded, query)
	n := int64(len(args))
	if v, ok := f.rows[query]; ok {
		n = v
	}
	return fakeResult{n: n, err: f.rowsErr[query]}, nil
}

// stmt builds a statement whose SQL names the table and chunk and whose
// argument count is the number of rows it deletes by default.
func stmt(table string, chunk string, nargs int) plan.Statement {
	args := make([]any, nargs)
	for i := range args {
		args[i] = i
	}
	return plan.Statement{SQL: "DELETE FROM `" + table + "` /*" + chunk + "*/", Args: args}
}

func step(table string, expected int64, stmts ...plan.Statement) plan.Step {
	return plan.Step{Table: table, Expected: expected, Statements: stmts}
}

// samplePlan: a non-cyclic group (c), a cyclic group (a, b), a non-cyclic
// group (root).
func samplePlan() *plan.Plan {
	return &plan.Plan{
		Groups: []plan.Group{
			{Steps: []plan.Step{step("c", 3, stmt("c", "1", 2), stmt("c", "2", 1))}},
			{Cyclic: true, Steps: []plan.Step{
				step("a", 1, stmt("a", "1", 1)),
				step("b", 2, stmt("b", "1", 2)),
			}},
			{Steps: []plan.Step{step("root", 1, stmt("root", "1", 1))}},
		},
		Total:      7,
		TableCount: 4,
	}
}

func TestRunOrderAndFKToggles(t *testing.T) {
	f := &fakeExec{}
	e := &Executor{Tx: f}
	res, err := e.Run(context.Background(), samplePlan())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantQ := []string{
		"DELETE FROM `c` /*1*/",
		"DELETE FROM `c` /*2*/",
		fkOff,
		"DELETE FROM `a` /*1*/",
		"DELETE FROM `b` /*1*/",
		fkOn,
		"DELETE FROM `root` /*1*/",
	}
	if !reflect.DeepEqual(f.queries, wantQ) {
		t.Errorf("queries:\n got %q\nwant %q", f.queries, wantQ)
	}
	wantR := []TableResult{
		{Table: "c", Expected: 3, Deleted: 3},
		{Table: "a", Expected: 1, Deleted: 1},
		{Table: "b", Expected: 2, Deleted: 2},
		{Table: "root", Expected: 1, Deleted: 1},
	}
	if !reflect.DeepEqual(res, wantR) {
		t.Errorf("results:\n got %+v\nwant %+v", res, wantR)
	}
}

func TestRunNonCyclicIssuesNoSet(t *testing.T) {
	p := &plan.Plan{Groups: []plan.Group{
		{Steps: []plan.Step{step("x", 1, stmt("x", "1", 1))}},
		{Steps: []plan.Step{step("y", 1, stmt("y", "1", 1))}},
	}}
	f := &fakeExec{}
	if _, err := (&Executor{Tx: f}).Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	for _, q := range f.queries {
		if strings.HasPrefix(q, "SET") {
			t.Errorf("unexpected %q", q)
		}
	}
	if len(f.queries) != 2 {
		t.Errorf("queries = %q", f.queries)
	}
}

func TestRunFailureInsideCyclicGroupRestoresChecks(t *testing.T) {
	boom := errors.New("boom")
	f := &fakeExec{execErr: map[string]error{"DELETE FROM `b` /*1*/": boom}}
	res, err := (&Executor{Tx: f}).Run(context.Background(), samplePlan())
	var ee *ExecError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *ExecError", err)
	}
	if ee.Table != "b" || !errors.Is(err, boom) {
		t.Errorf("ExecError = %+v", ee)
	}
	if !strings.Contains(err.Error(), "b") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("message %q lacks table or cause", err.Error())
	}
	wantQ := []string{
		"DELETE FROM `c` /*1*/",
		"DELETE FROM `c` /*2*/",
		fkOff,
		"DELETE FROM `a` /*1*/",
		"DELETE FROM `b` /*1*/",
		fkOn,
	}
	if !reflect.DeepEqual(f.queries, wantQ) {
		t.Errorf("queries:\n got %q\nwant %q", f.queries, wantQ)
	}
	wantR := []TableResult{
		{Table: "c", Expected: 3, Deleted: 3},
		{Table: "a", Expected: 1, Deleted: 1},
		{Table: "b", Expected: 2, Deleted: 0},
	}
	if !reflect.DeepEqual(res, wantR) {
		t.Errorf("partial results:\n got %+v\nwant %+v", res, wantR)
	}
}

func TestRunRestoreFailureIsJoined(t *testing.T) {
	boom := errors.New("boom")
	restore := errors.New("restore failed")
	f := &fakeExec{execErr: map[string]error{
		"DELETE FROM `a` /*1*/": boom,
		fkOn:                    restore,
	}}
	_, err := (&Executor{Tx: f}).Run(context.Background(), samplePlan())
	var ee *ExecError
	if !errors.As(err, &ee) || ee.Table != "a" {
		t.Fatalf("err = %v, want *ExecError for a", err)
	}
	if !errors.Is(err, boom) || !errors.Is(err, restore) {
		t.Errorf("err %v does not carry both causes", err)
	}
	if got := f.queries[len(f.queries)-1]; got != fkOn {
		t.Errorf("last query = %q, want %q", got, fkOn)
	}
}

func TestRunRestoreFailureAfterSuccess(t *testing.T) {
	restore := errors.New("restore failed")
	f := &fakeExec{execErr: map[string]error{fkOn: restore}}
	res, err := (&Executor{Tx: f}).Run(context.Background(), samplePlan())
	var ee *ExecError
	if !errors.As(err, &ee) || ee.Table != "b" || !errors.Is(err, restore) {
		t.Fatalf("err = %v, want *ExecError for the group's last table", err)
	}
	if len(f.queries) != 6 {
		t.Errorf("execution continued after restore failure: %q", f.queries)
	}
	msg := err.Error()
	if !strings.Contains(msg, "re-enable foreign_key_checks after group ending at table b") || strings.Contains(msg, "delete") {
		t.Errorf("message %q does not say only the re-enable failed", msg)
	}
	if len(res) != 3 {
		t.Errorf("results = %+v", res)
	}
}

func TestRunDisableFailure(t *testing.T) {
	denied := errors.New("denied")
	f := &fakeExec{execErr: map[string]error{fkOff: denied}}
	res, err := (&Executor{Tx: f}).Run(context.Background(), samplePlan())
	var ee *ExecError
	if !errors.As(err, &ee) || ee.Table != "a" || !errors.Is(err, denied) {
		t.Fatalf("err = %v, want *ExecError for a wrapping denied", err)
	}
	for _, q := range f.queries[3:] {
		if strings.HasPrefix(q, "DELETE") {
			t.Errorf("delete issued after disable failure: %q", q)
		}
	}
	if len(res) != 1 || res[0].Table != "c" {
		t.Errorf("results = %+v", res)
	}
}

func TestRunRowsAffectedError(t *testing.T) {
	bad := errors.New("no rows info")
	f := &fakeExec{rowsErr: map[string]error{"DELETE FROM `c` /*2*/": bad}}
	res, err := (&Executor{Tx: f}).Run(context.Background(), samplePlan())
	var ee *ExecError
	if !errors.As(err, &ee) || ee.Table != "c" || !errors.Is(err, bad) {
		t.Fatalf("err = %v", err)
	}
	if len(f.queries) != 2 {
		t.Errorf("queries = %q", f.queries)
	}
	if want := []TableResult{{Table: "c", Expected: 3, Deleted: 2}}; !reflect.DeepEqual(res, want) {
		t.Errorf("results = %+v", res)
	}
}

func TestRunCountMismatch(t *testing.T) {
	f := &fakeExec{rows: map[string]int64{
		"DELETE FROM `c` /*2*/":    0, // c: 2 of 3
		"DELETE FROM `root` /*1*/": 5, // root: 5 of 1
	}}
	res, err := (&Executor{Tx: f}).Run(context.Background(), samplePlan())
	var cm *CountMismatchError
	if !errors.As(err, &cm) {
		t.Fatalf("err = %v, want *CountMismatchError", err)
	}
	want := []TableResult{
		{Table: "c", Expected: 3, Deleted: 2},
		{Table: "root", Expected: 1, Deleted: 5},
	}
	if !reflect.DeepEqual(cm.Mismatches, want) {
		t.Errorf("mismatches = %+v", cm.Mismatches)
	}
	if len(res) != 4 {
		t.Errorf("results = %+v, want all 4", res)
	}
	msg := err.Error()
	for _, s := range []string{"c", "expected 3", "deleted 2", "root", "expected 1", "deleted 5"} {
		if !strings.Contains(msg, s) {
			t.Errorf("message %q lacks %q", msg, s)
		}
	}
	if strings.Contains(msg, "`a`") || strings.Contains(msg, " b:") {
		t.Errorf("message %q lists matching tables", msg)
	}
}

func TestRunProgressIsCumulative(t *testing.T) {
	type call struct {
		table string
		n     int64
	}
	var got []call
	e := &Executor{Tx: &fakeExec{}, Progress: func(table string, n int64) { got = append(got, call{table, n}) }}
	if _, err := e.Run(context.Background(), samplePlan()); err != nil {
		t.Fatal(err)
	}
	want := []call{{"c", 2}, {"c", 3}, {"a", 1}, {"b", 2}, {"root", 1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("progress = %+v, want %+v", got, want)
	}
}

func TestRunContextCanceled(t *testing.T) {
	// cancel is triggered while the cyclic group runs: while a's statement
	// is in flight (so it fails, as on a real connection), and from the
	// Progress callback after a's statement succeeded (so b is never tried).
	for _, tc := range []struct {
		name       string
		byProgress bool
		failTable  string
		forbidden  []string
	}{
		{"during statement", false, "a", []string{"DELETE FROM `b` /*1*/", "DELETE FROM `root` /*1*/"}},
		{"from progress", true, "b", []string{"DELETE FROM `b` /*1*/", "DELETE FROM `root` /*1*/"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := &fakeExec{}
			e := &Executor{Tx: f}
			if tc.byProgress {
				e.Progress = func(table string, _ int64) {
					if table == "a" {
						cancel()
					}
				}
			} else {
				f.onExec = func(q string) {
					if q == "DELETE FROM `a` /*1*/" {
						cancel()
					}
				}
			}
			res, err := e.Run(ctx, samplePlan())
			var ee *ExecError
			if !errors.As(err, &ee) || ee.Table != tc.failTable || !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want *ExecError for %s wrapping context.Canceled", err, tc.failTable)
			}
			if strings.Contains(err.Error(), "re-enable") {
				t.Errorf("err %q reports a restore failure", err)
			}
			for _, q := range f.queries {
				if slices.Contains(tc.forbidden, q) {
					t.Errorf("statement %q attempted after cancellation", q)
				}
			}
			if got := f.succeeded[len(f.succeeded)-1]; got != fkOn {
				t.Errorf("last successful query = %q, want %q (succeeded: %q)", got, fkOn, f.succeeded)
			}
			if last := res[len(res)-1]; last.Table != tc.failTable {
				t.Errorf("results = %+v, want to end at %s", res, tc.failTable)
			}
		})
	}
}

func TestRunCanceledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeExec{}
	_, err := (&Executor{Tx: f}).Run(ctx, samplePlan())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(f.queries) != 0 {
		t.Errorf("queries = %q", f.queries)
	}
}

func TestRunEmptyPlan(t *testing.T) {
	f := &fakeExec{}
	res, err := (&Executor{Tx: f}).Run(context.Background(), &plan.Plan{})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || len(res) != 0 {
		t.Errorf("results = %#v, want empty non-nil", res)
	}
	if len(f.queries) != 0 {
		t.Errorf("queries = %q", f.queries)
	}
}
