package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/yamagame/mysql-relation-deleter/internal/collect"
	"github.com/yamagame/mysql-relation-deleter/internal/dbconn"
	"github.com/yamagame/mysql-relation-deleter/internal/schema"
	"github.com/yamagame/mysql-relation-deleter/internal/sqlstore"
)

// flowSchema is a two-table schema: orders.user_id -> users.id.
const flowSchema = `{"format_version": 1, "database": "shop", "tables": [
  {"name": "users", "columns": [{"name": "id", "type": "int", "nullable": false}], "primary_key": ["id"]},
  {"name": "orders", "columns": [
    {"name": "id", "type": "int", "nullable": false},
    {"name": "user_id", "type": "int", "nullable": false}], "primary_key": ["id"]}],
 "foreign_keys": [{"name": "fk_orders_user", "table": "orders", "columns": ["user_id"],
   "delete_rule": "RESTRICT", "referenced_table": "users", "referenced_columns": ["id"]}]}`

// flowData is the content of the fake database: users 1 and 2; user 1 has
// orders 10 and 11, user 2 has order 20.
func flowData() map[string][]map[string]string {
	return map[string][]map[string]string{
		"users": {{"id": "1"}, {"id": "2"}},
		"orders": {
			{"id": "10", "user_id": "1"},
			{"id": "11", "user_id": "1"},
			{"id": "20", "user_id": "2"},
		},
	}
}

// memSource is a scripted collect.RowSource over flowData. It returns the
// rows of table whose predicate columns match one of the predicate tuples.
type memSource struct {
	data    map[string][]map[string]string
	err     map[string]error
	fetches int
}

func (m *memSource) Fetch(ctx context.Context, table string, columns []string, keyed bool, p collect.Predicate) ([]collect.Row, error) {
	m.fetches++
	if err := m.err[table]; err != nil {
		return nil, err
	}
	var out []collect.Row
	for _, r := range m.data[table] {
		for _, want := range p.Values {
			match := true
			for i, c := range p.Columns {
				if r[c] != want[i] {
					match = false
					break
				}
			}
			if match {
				vals := make(collect.Tuple, len(columns))
				for i, c := range columns {
					vals[i] = r[c]
				}
				out = append(out, collect.Row{Values: vals, Count: 1})
				break
			}
		}
	}
	return out, nil
}

// fakeTx records every call. ExecContext and Commit must never be called by
// the dry-run.
type fakeTx struct {
	queries, execs, commits, rollbacks int
}

func (t *fakeTx) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	t.queries++
	return nil, errors.New("fakeTx: QueryContext is not scripted; use the fake RowSource")
}

func (t *fakeTx) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	t.execs++
	return nil, errors.New("fakeTx: ExecContext must not be called")
}

func (t *fakeTx) Commit() error   { t.commits++; return nil }
func (t *fakeTx) Rollback() error { t.rollbacks++; return nil }

// fakeDB is a TxBeginner and io.Closer that records BeginTx calls.
type fakeDB struct {
	tx       *fakeTx
	beginErr error
	opts     []*sql.TxOptions
	ctxs     []context.Context
	closed   int
}

func (d *fakeDB) BeginTx(ctx context.Context, opts *sql.TxOptions) (Tx, error) {
	d.opts = append(d.opts, opts)
	d.ctxs = append(d.ctxs, ctx)
	if d.beginErr != nil {
		return nil, d.beginErr
	}
	return d.tx, nil
}

func (d *fakeDB) Close() error { d.closed++; return nil }

// flowEnv is the fake environment of one Run.
type flowEnv struct {
	db      *fakeDB
	src     *memSource
	openErr error
	env     map[string]string

	configs  []dbconn.Config
	sources  int
	chunkArg int
}

func newFlowEnv() *flowEnv {
	return &flowEnv{
		db:  &fakeDB{tx: &fakeTx{}},
		src: &memSource{data: flowData()},
		env: map[string]string{},
	}
}

func (e *flowEnv) deps() Deps {
	return Deps{
		Open: func(ctx context.Context, c dbconn.Config) (TxBeginner, error) {
			e.configs = append(e.configs, c)
			if e.openErr != nil {
				return nil, e.openErr
			}
			return e.db, nil
		},
		NewRowSource: func(q sqlstore.Querier, s *schema.Schema, chunkSize int) collect.RowSource {
			e.sources++
			e.chunkArg = chunkSize
			if q != Tx(e.db.tx) {
				panic("NewRowSource did not get the transaction")
			}
			return e.src
		},
	}
}

type flowResult struct {
	code        int
	out, errOut string
}

func (e *flowEnv) run(ctx context.Context, args ...string) flowResult {
	var out, errb bytes.Buffer
	io := IO{
		In:         strings.NewReader(""),
		Out:        &out,
		Err:        &errb,
		IsTerminal: func() bool { return false },
		Getenv:     func(k string) string { return e.env[k] },
	}
	code := Run(ctx, args, io, e.deps())
	return flowResult{code, out.String(), errb.String()}
}

func flowArgs(t *testing.T, extra ...string) []string {
	t.Helper()
	p := writeTemp(t, "schema.json", flowSchema)
	return append([]string{"--schema", p, "--table", "users", "-u", "app"}, extra...)
}

// assertReadOnly checks that the dry-run opened exactly one read-only
// transaction, never wrote or committed, rolled back once and closed the
// connection.
func assertReadOnly(t *testing.T, e *flowEnv) {
	t.Helper()
	if len(e.db.opts) != 1 {
		t.Fatalf("BeginTx called %d times, want 1", len(e.db.opts))
	}
	if o := e.db.opts[0]; o == nil || !o.ReadOnly {
		t.Errorf("BeginTx opts = %+v, want ReadOnly", o)
	}
	tx := e.db.tx
	if tx.execs != 0 {
		t.Errorf("ExecContext called %d times, want 0", tx.execs)
	}
	if tx.commits != 0 {
		t.Errorf("Commit called %d times, want 0", tx.commits)
	}
	if tx.rollbacks != 1 {
		t.Errorf("Rollback called %d times, want 1", tx.rollbacks)
	}
	if e.db.closed != 1 {
		t.Errorf("Close called %d times, want 1", e.db.closed)
	}
}

func TestDryRunSuccess(t *testing.T) {
	e := newFlowEnv()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := e.run(ctx, flowArgs(t, "--id", "1", "--chunk-size", "7")...)
	if r.code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr:\n%s", r.code, r.errOut)
	}
	assertReadOnly(t, e)
	if e.db.ctxs[0].Done() != nil {
		t.Errorf("BeginTx got a cancellable context; want context.WithoutCancel")
	}
	if e.sources != 1 || e.chunkArg != 7 {
		t.Errorf("NewRowSource calls = %d, chunk size = %d; want 1, 7", e.sources, e.chunkArg)
	}
	for _, w := range []string{
		"Deletion plan (child -> parent):", "orders", "users",
		"DELETE FROM `orders`", "DELETE FROM `users`", "total=3 tables=2",
	} {
		if !strings.Contains(r.out, w) {
			t.Errorf("plan does not contain %q\nstdout:\n%s", w, r.out)
		}
	}
	if strings.Contains(r.out, "'20'") {
		t.Errorf("plan contains the order of user 2:\n%s", r.out)
	}
	if !strings.Contains(r.errOut, "collect users: 1 rows") || !strings.Contains(r.errOut, "collect orders: 2 rows") {
		t.Errorf("stderr lacks the collect progress:\n%s", r.errOut)
	}
	if strings.Contains(r.errOut, "warning") {
		t.Errorf("unexpected warning:\n%s", r.errOut)
	}
}

func TestDryRunOpenFailure(t *testing.T) {
	e := newFlowEnv()
	e.env["MYSQL_PWD"] = "s3cret"
	e.openErr = fmt.Errorf("connect to app@db.example:3307/shop: dial tcp: connection refused")
	r := e.run(context.Background(), flowArgs(t, "--id", "1", "-h", "db.example", "-P", "3307")...)
	if r.code != 1 {
		t.Errorf("exit code = %d, want 1", r.code)
	}
	want := "relation-deleter: connect to app@db.example:3307/shop: dial tcp: connection refused\n"
	if r.errOut != want {
		t.Errorf("stderr = %q, want %q", r.errOut, want)
	}
	if strings.Contains(r.errOut+r.out, "s3cret") {
		t.Errorf("output contains the password")
	}
	if len(e.db.opts) != 0 {
		t.Errorf("BeginTx called after a failed Open")
	}
}

func TestDryRunResolveFailure(t *testing.T) {
	e := newFlowEnv()
	p := writeTemp(t, "schema.json", flowSchema)
	r := e.run(context.Background(), "--schema", p, "--table", "users", "--id", "1")
	if r.code != 2 {
		t.Errorf("exit code = %d, want 2\nstderr:\n%s", r.code, r.errOut)
	}
	if !strings.HasPrefix(r.errOut, "relation-deleter: no user given") {
		t.Errorf("stderr = %q", r.errOut)
	}
	if len(e.configs) != 0 {
		t.Errorf("Open called with an invalid configuration")
	}

	t.Run("unreadable option file", func(t *testing.T) {
		e := newFlowEnv()
		r := e.run(context.Background(), flowArgs(t, "--id", "1", "--defaults-file", t.TempDir()+"/none.cnf")...)
		if r.code != 2 || len(e.configs) != 0 {
			t.Errorf("exit code = %d, opens = %d; want 2, 0\nstderr:\n%s", r.code, len(e.configs), r.errOut)
		}
	})
}

func TestDryRunDatabaseFallback(t *testing.T) {
	t.Run("schema database when -D is absent", func(t *testing.T) {
		e := newFlowEnv()
		if r := e.run(context.Background(), flowArgs(t, "--id", "1")...); r.code != 0 {
			t.Fatalf("exit code = %d\nstderr:\n%s", r.code, r.errOut)
		}
		if len(e.configs) != 1 || e.configs[0].Database != "shop" {
			t.Errorf("Open configs = %v, want database shop", e.configs)
		}
	})
	for _, db := range []string{"other", ""} {
		t.Run(fmt.Sprintf("flag %q wins", db), func(t *testing.T) {
			e := newFlowEnv()
			if r := e.run(context.Background(), flowArgs(t, "--id", "1", "-D", db)...); r.code != 0 {
				t.Fatalf("exit code = %d\nstderr:\n%s", r.code, r.errOut)
			}
			if len(e.configs) != 1 || e.configs[0].Database != db {
				t.Errorf("Open configs = %v, want database %q", e.configs, db)
			}
		})
	}
}

func TestDryRunPrintsResolveWarnings(t *testing.T) {
	e := newFlowEnv()
	cnf := writeTemp(t, "my.cnf", "[client]\nuser=app\n")
	if err := os.Chmod(cnf, 0o644); err != nil {
		t.Fatal(err)
	}
	p := writeTemp(t, "schema.json", flowSchema)
	r := e.run(context.Background(), "--schema", p, "--table", "users", "--id", "1", "--defaults-file", cnf)
	if r.code != 0 {
		t.Fatalf("exit code = %d\nstderr:\n%s", r.code, r.errOut)
	}
	want := "relation-deleter: warning: option file " + cnf + " has permissions 0644"
	if !strings.Contains(r.errOut, want) {
		t.Errorf("stderr lacks %q:\n%s", want, r.errOut)
	}
}

func TestDryRunMissingRoots(t *testing.T) {
	t.Run("partial", func(t *testing.T) {
		e := newFlowEnv()
		r := e.run(context.Background(), flowArgs(t, "--id", "1", "--id", "99")...)
		if r.code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr:\n%s", r.code, r.errOut)
		}
		if !strings.Contains(r.errOut, "warning: root record not found: '99'") {
			t.Errorf("stderr lacks the missing-root warning:\n%s", r.errOut)
		}
		if !strings.Contains(r.out, "total=3 tables=2") {
			t.Errorf("plan:\n%s", r.out)
		}
		assertReadOnly(t, e)
	})
	t.Run("all", func(t *testing.T) {
		e := newFlowEnv()
		r := e.run(context.Background(), flowArgs(t, "--id", "98", "--id", "99")...)
		if r.code != 2 {
			t.Fatalf("exit code = %d, want 2\nstderr:\n%s", r.code, r.errOut)
		}
		for _, w := range []string{
			"root record not found: '98'", "root record not found: '99'",
			"relation-deleter: no matching records in users\n",
		} {
			if !strings.Contains(r.errOut, w) {
				t.Errorf("stderr lacks %q:\n%s", w, r.errOut)
			}
		}
		if r.out != "" {
			t.Errorf("stdout = %q, want empty", r.out)
		}
		assertReadOnly(t, e)
	})
}

func TestDryRunMaxRecords(t *testing.T) {
	tests := []struct {
		limit    string
		wantCode int
	}{
		{"2", 3},
		{"3", 0},
		{"4", 0},
		{"0", 0},
	}
	for _, tt := range tests {
		t.Run("limit "+tt.limit, func(t *testing.T) {
			e := newFlowEnv()
			r := e.run(context.Background(), flowArgs(t, "--id", "1", "--max-records", tt.limit)...)
			if r.code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d\nstderr:\n%s", r.code, tt.wantCode, r.errOut)
			}
			assertReadOnly(t, e)
			if tt.wantCode == 3 {
				want := "relation-deleter: 3 records exceed --max-records 2; nothing deleted\n"
				if !strings.HasSuffix(r.errOut, want) {
					t.Errorf("stderr = %q, want suffix %q", r.errOut, want)
				}
				if r.out != "" {
					t.Errorf("plan rendered despite the limit:\n%s", r.out)
				}
				return
			}
			if !strings.Contains(r.out, "total=3 tables=2") {
				t.Errorf("plan:\n%s", r.out)
			}
		})
	}
}

func TestDryRunRuntimeErrors(t *testing.T) {
	t.Run("collect error", func(t *testing.T) {
		e := newFlowEnv()
		e.src.err = map[string]error{"orders": errors.New("query orders: lock wait timeout")}
		r := e.run(context.Background(), flowArgs(t, "--id", "1")...)
		if r.code != 1 {
			t.Fatalf("exit code = %d, want 1\nstderr:\n%s", r.code, r.errOut)
		}
		if !strings.Contains(r.errOut, "relation-deleter: ") || !strings.Contains(r.errOut, "lock wait timeout") {
			t.Errorf("stderr = %q", r.errOut)
		}
		if r.out != "" {
			t.Errorf("stdout = %q, want empty", r.out)
		}
		assertReadOnly(t, e)
	})
	t.Run("BeginTx error", func(t *testing.T) {
		e := newFlowEnv()
		e.db.beginErr = errors.New("driver: bad connection")
		r := e.run(context.Background(), flowArgs(t, "--id", "1")...)
		if r.code != 1 {
			t.Fatalf("exit code = %d, want 1\nstderr:\n%s", r.code, r.errOut)
		}
		if !strings.Contains(r.errOut, "bad connection") {
			t.Errorf("stderr = %q", r.errOut)
		}
		if e.sources != 0 || e.db.closed != 1 {
			t.Errorf("sources = %d, closed = %d; want 0, 1", e.sources, e.db.closed)
		}
	})
}

func TestExecuteReachesBoundary(t *testing.T) {
	e := newFlowEnv()
	r := e.run(context.Background(), flowArgs(t, "--id", "1", "--execute", "--yes")...)
	if r.code != 1 {
		t.Errorf("exit code = %d, want 1", r.code)
	}
	if want := "relation-deleter: --execute is not available in this build\n"; r.errOut != want {
		t.Errorf("stderr = %q, want %q", r.errOut, want)
	}
	if len(e.db.opts) != 0 || e.sources != 0 {
		t.Errorf("BeginTx calls = %d, NewRowSource calls = %d; want 0, 0", len(e.db.opts), e.sources)
	}
	if r.out != "" {
		t.Errorf("stdout = %q, want empty", r.out)
	}
}
