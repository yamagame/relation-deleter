package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/yamagame/mysql-relation-deleter/internal/collect"
	"github.com/yamagame/mysql-relation-deleter/internal/dbconn"
	"github.com/yamagame/mysql-relation-deleter/internal/schema"
	"github.com/yamagame/mysql-relation-deleter/internal/sqlstore"
)

const (
	goldenSchema    = "../../testdata/schema.golden.8.0.json"
	sampleRelations = "../../testdata/relations.sample.yaml"
)

// Compile-time checks of the production adapter.
var (
	_ TxBeginner = (*sqlDB)(nil)
	_ Tx         = (*sql.Tx)(nil)
)

// fakeDeps records every Open call. It never succeeds: no test in this file
// may reach the database.
type fakeDeps struct {
	t      *testing.T
	opened int
}

func (f *fakeDeps) deps() Deps {
	return Deps{
		Open: func(ctx context.Context, c dbconn.Config) (TxBeginner, error) {
			f.opened++
			f.t.Errorf("deps.Open called with %s", c.Redacted())
			return nil, context.Canceled
		},
		NewRowSource: func(q sqlstore.Querier, s *schema.Schema, chunkSize int) collect.RowSource {
			f.t.Errorf("deps.NewRowSource called")
			return nil
		},
	}
}

func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string, opened int) {
	t.Helper()
	f := &fakeDeps{t: t}
	var out, errb bytes.Buffer
	io := IO{
		In:         strings.NewReader(""),
		Out:        &out,
		Err:        &errb,
		IsTerminal: func() bool { return false },
		Getenv:     func(string) string { return "" },
	}
	code = Run(context.Background(), args, io, f.deps())
	return code, out.String(), errb.String(), f.opened
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunRejectsInvalidInputBeforeDB(t *testing.T) {
	badJSON := writeTemp(t, "bad.json", "{not json")
	badVersion := writeTemp(t, "ver.json", `{"format_version": 9, "database": "x", "tables": [], "foreign_keys": []}`)
	badRelations := writeTemp(t, "bad.yaml", `version: 1
relations:
  - child:  { table: nope, columns: [a] }
    parent: { table: users, columns: [id] }
  - child:  { table: orders, columns: [user_id, id] }
    parent: { table: users, columns: [id] }
`)
	base := []string{"--schema", goldenSchema}

	tests := []struct {
		name string
		args []string
		want []string // substrings of stderr
	}{
		{"unknown flag", []string{"--bogus"}, []string{"flag provided but not defined: -bogus", "--help"}},
		{"missing all required", nil, []string{"--schema is required", "--table is required", "at least one --id is required", "--help"}},
		{"missing id", append(base, "--table", "users"), []string{"at least one --id is required"}},
		{"missing schema", []string{"--table", "users", "--id", "1"}, []string{"--schema is required"}},
		{"negative max-records", append(base, "--table", "users", "--id", "1", "--max-records", "-1"), []string{"--max-records must be 0 or greater"}},
		{"zero chunk-size", append(base, "--table", "users", "--id", "1", "--chunk-size", "0"), []string{"--chunk-size must be greater than 0"}},
		{"non-numeric port", append(base, "--table", "users", "--id", "1", "-P", "abc"), []string{"invalid value"}},
		{"positional args", append(base, "--table", "users", "--id", "1", "extra"), []string{`unexpected argument "extra"`}},
		{"short password flag", append(base, "--table", "users", "--id", "1", "-p"), []string{"no password flag", "MYSQL_PWD", "--defaults-file"}},
		{"long password flag", append(base, "--table", "users", "--id", "1", "--password=secret"), []string{"no password flag", "MYSQL_PWD"}},
		{"attached password", append(base, "--table", "users", "--id", "1", "-psecret"), []string{"no password flag"}},
		{"unreadable schema", []string{"--schema", filepath.Join(t.TempDir(), "none.json"), "--table", "users", "--id", "1"}, []string{"cannot read file"}},
		{"invalid schema JSON", []string{"--schema", badJSON, "--table", "users", "--id", "1"}, []string{"invalid JSON"}},
		{"invalid schema structure", []string{"--schema", badVersion, "--table", "users", "--id", "1"}, []string{"unsupported format_version"}},
		{"invalid relations", append(base, "--relations", badRelations, "--table", "users", "--id", "1"),
			[]string{badRelations + ":3:", badRelations + ":5:"}},
		{"unreadable relations", append(base, "--relations", filepath.Join(t.TempDir(), "none.yaml"), "--table", "users", "--id", "1"),
			[]string{"relations file", "cannot read file"}},
		{"unknown table", append(base, "--table", "nope", "--id", "1"), []string{`table "nope" not found in schema file`}},
		{"table without PK", append(base, "--table", "audit_logs", "--id", "1"), []string{`table "audit_logs" has no primary key`}},
		{"too few values for composite PK", append(base, "--table", "shipments", "--id", "1,2", "--id", "7"),
			[]string{`--id "7": table "shipments" has 2 primary key columns (order_id, seq), got 1 value`}},
		{"too many values", append(base, "--table", "users", "--id", "1,2"),
			[]string{`--id "1,2": table "users" has 1 primary key column (id), got 2 values`}},
		{"malformed CSV id", append(base, "--table", "users", "--id", `"1`), []string{`--id "\"1"`}},
		{"empty id", append(base, "--table", "users", "--id", ""), []string{`--id "": empty value`}},
		{"odd-length hex on binary PK", append(base, "--table", "devices", "--id", "0x123"),
			[]string{`--id "0x123": column "device_uuid": invalid hex value`, "odd length"}},
		{"non-hex chars on binary PK", append(base, "--table", "devices", "--id", "0xZZ", "--id", "0X0g"),
			[]string{`--id "0xZZ": column "device_uuid": invalid hex value`, `--id "0X0g": column "device_uuid": invalid hex value`}},
		{"password-like value of an int flag", append(base, "--table", "users", "--id", "1", "-P", "-psecret"), []string{"no password flag"}},
		{"password-like value of an int flag with =", append(base, "--table", "users", "--id", "1", "--max-records=--password=secret"), []string{"no password flag"}},
		{"password-like value of a bool flag", append(base, "--table", "users", "--id", "1", "--execute=-psecret"), []string{"no password flag"}},
		{"bad table and bad relations together", append(base, "--relations", badRelations, "--table", "nope", "--id", "1"),
			[]string{badRelations + ":3:", `table "nope" not found`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr, opened := runCLI(t, tt.args...)
			if code != 2 {
				t.Errorf("exit code = %d, want 2\nstderr:\n%s", code, stderr)
			}
			if opened != 0 {
				t.Errorf("deps.Open called %d times, want 0", opened)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			for _, w := range tt.want {
				if !strings.Contains(stderr, w) {
					t.Errorf("stderr does not contain %q\nstderr:\n%s", w, stderr)
				}
			}
			for _, line := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
				if !strings.HasPrefix(line, "relation-deleter: ") {
					t.Errorf("stderr line %q lacks the relation-deleter: prefix", line)
				}
			}
			if strings.Contains(stderr, "secret") {
				t.Errorf("stderr echoes the password: %s", stderr)
			}
		})
	}
}

func TestRunHelp(t *testing.T) {
	for _, arg := range []string{"--help", "-help"} {
		t.Run(arg, func(t *testing.T) {
			code, stdout, stderr, opened := runCLI(t, arg)
			if code != 0 {
				t.Errorf("exit code = %d, want 0", code)
			}
			if opened != 0 {
				t.Errorf("deps.Open called")
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want empty", stderr)
			}
			for _, w := range []string{
				"Usage:", "--schema", "--relations", "--table", "--id", "--execute", "--yes",
				"--max-records", "--chunk-size", "-h, --host", "-P, --port", "-u, --user",
				"-D, --database", "--socket", "--defaults-file", "--help",
				"MYSQL_PWD", "[client]", "no password flag", "0x", "right-padded with 0x00",
				"dry-run", "full table scan", "FLOAT", "JSON", "without a foreign key", "other databases",
				"Exit codes:", "  0 ", "  1 ", "  2 ", "  3 ",
			} {
				if !strings.Contains(stdout, w) {
					t.Errorf("usage does not contain %q", w)
				}
			}
		})
	}
}

// TestRunValidInputReachesOpen checks that valid input passes every check
// before the database and reaches deps.Open exactly once.
func TestRunValidInputReachesOpen(t *testing.T) {
	tests := [][]string{
		{"--schema", goldenSchema, "--table", "users", "--id", "1", "-u", "app"},
		{"--schema", goldenSchema, "--relations", sampleRelations, "--table", "users", "--id", "1", "--id", "2",
			"--execute", "--yes", "--max-records", "10", "--chunk-size", "100",
			"-h", "db.example", "-P", "3307", "-u", "app", "-D", "app", "--socket", "/tmp/s"},
		{"-schema", goldenSchema, "-table", "shipments", "-id", "1,2", "--id", `3,"4"`, "-u", "app"},
		// A string value that looks like -p is passed through: it cannot make
		// the flag parser fail, so it is never echoed in a parse error.
		{"--schema", goldenSchema, "--table", "users", "--id", "-pfoo", "-u", "app"},
		{"--schema", goldenSchema, "--table", "devices", "--id", "0x000102030405060708090a0b0c0d0e0f", "-u", "app"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			opened := 0
			deps := Deps{
				Open: func(ctx context.Context, c dbconn.Config) (TxBeginner, error) {
					opened++
					return nil, errors.New("connect to " + c.Redacted() + ": refused")
				},
				NewRowSource: func(q sqlstore.Querier, s *schema.Schema, chunkSize int) collect.RowSource {
					t.Errorf("deps.NewRowSource called")
					return nil
				},
			}
			var out, errb bytes.Buffer
			code := Run(context.Background(), args, IO{Out: &out, Err: &errb, Getenv: func(string) string { return "" }}, deps)
			if code != 1 {
				t.Errorf("exit code = %d, want 1\nstderr:\n%s", code, errb.String())
			}
			if opened != 1 {
				t.Errorf("deps.Open called %d times, want 1", opened)
			}
			if out.Len() != 0 {
				t.Errorf("stdout = %q, want empty", out.String())
			}
			if !strings.HasPrefix(errb.String(), "relation-deleter: connect to app@") {
				t.Errorf("stderr = %q", errb.String())
			}
		})
	}
}

func TestParseFlagsFillsInvocation(t *testing.T) {
	var errb bytes.Buffer
	inv, code := parseInvocation([]string{
		"--schema", goldenSchema, "--relations", sampleRelations, "--table", "shipments",
		"--id", "1,2", "--id", `"1","2"`, "--id", "3,4", "--execute", "--yes",
		"--max-records", "10", "--chunk-size", "100", "-h", "db", "--port", "3307", "-u", "app", "--defaults-file", "my.cnf",
	}, IO{Out: &bytes.Buffer{}, Err: &errb})
	if code != proceed {
		t.Fatalf("code = %d, want -1 (continue); stderr:\n%s", code, errb.String())
	}
	if inv.table != "shipments" || !inv.execute || !inv.yes || inv.maxRecords != 10 || inv.chunkSize != 100 {
		t.Errorf("unexpected invocation: %+v", inv)
	}
	if want := []collect.Tuple{{"1", "2"}, {"3", "4"}}; !reflect.DeepEqual(inv.ids, want) {
		t.Errorf("ids = %#v, want %#v", inv.ids, want)
	}
	if inv.schema == nil || inv.graph == nil || len(inv.relations) != 1 {
		t.Errorf("schema/graph/relations not loaded: %+v", inv)
	}
	c := inv.conn
	if c.Host == nil || *c.Host != "db" || c.Port == nil || *c.Port != 3307 || c.User == nil || *c.User != "app" {
		t.Errorf("conn flags = %+v", c)
	}
	if c.Database != nil || c.Socket != nil {
		t.Errorf("unset flags must stay nil: %+v", c)
	}
	if c.DefaultsFile != "my.cnf" {
		t.Errorf("DefaultsFile = %q", c.DefaultsFile)
	}
}

func TestParseFlagsDefaults(t *testing.T) {
	var errb bytes.Buffer
	inv, code := parseInvocation([]string{"--schema", goldenSchema, "--table", "users", "--id", "1"}, IO{Out: &bytes.Buffer{}, Err: &errb})
	if code != proceed {
		t.Fatalf("code = %d; stderr:\n%s", code, errb.String())
	}
	if inv.execute || inv.yes || inv.maxRecords != 0 || inv.chunkSize != 500 {
		t.Errorf("defaults = %+v", inv)
	}
	if inv.relations != nil {
		t.Errorf("relations = %v, want nil without --relations", inv.relations)
	}
	c := inv.conn
	if c.Host != nil || c.Port != nil || c.User != nil || c.Database != nil || c.Socket != nil || c.DefaultsFile != "" {
		t.Errorf("conn flags must be unset: %+v", c)
	}
}

func TestParseID(t *testing.T) {
	tests := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{"1", []string{"1"}, false},
		{"abc", []string{"abc"}, false},
		{"10,20", []string{"10", "20"}, false},
		{`10,"a,b"`, []string{"10", "a,b"}, false},
		{`"say ""hi""",x`, []string{`say "hi"`, "x"}, false},
		{` 1, 2`, []string{" 1", " 2"}, false},
		{`1,`, []string{"1", ""}, false},
		{"", nil, true},
		{`"1`, nil, true},
		{"1\n2", nil, true},
	}
	for _, tt := range tests {
		got, err := parseID(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseID(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseID(%q) = %#v, want %#v", tt.in, got, tt.want)
		}
	}
}

func TestDedupeIDs(t *testing.T) {
	in := []parsedID{
		{"0x0102", collect.Tuple{[]byte{1, 2}}},
		{"[1 2]", collect.Tuple{"[1 2]"}},
		{"0x6162", collect.Tuple{[]byte("ab")}},
		{"ab", collect.Tuple{"ab"}},
		{"0X0102", collect.Tuple{[]byte{1, 2}}},
		{"0x", collect.Tuple{[]byte{}}},
		{`""`, collect.Tuple{""}},
	}
	want := []parsedID{in[0], in[1], in[2], in[3], in[5], in[6]}
	if got := dedupeIDs(in); !reflect.DeepEqual(got, want) {
		t.Errorf("dedupeIDs across types = %#v, want %#v", got, want)
	}

	in = []parsedID{
		{"2", collect.Tuple{"2"}},
		{"1", collect.Tuple{"1"}},
		{`"2"`, collect.Tuple{"2"}},
		{`"a,b",c`, collect.Tuple{"a,b", "c"}},
		{`a,"b,c"`, collect.Tuple{"a", "b,c"}},
		{`"a,b","c"`, collect.Tuple{"a,b", "c"}},
		{"1", collect.Tuple{"1"}},
		{",", collect.Tuple{"", ""}},
		{`""`, collect.Tuple{""}},
	}
	want = []parsedID{in[0], in[1], in[3], in[4], in[7], in[8]}
	if got := dedupeIDs(in); !reflect.DeepEqual(got, want) {
		t.Errorf("dedupeIDs = %#v, want %#v", got, want)
	}
}

func TestDefaultDeps(t *testing.T) {
	d := DefaultDeps()
	if d.Open == nil || d.NewRowSource == nil {
		t.Fatalf("DefaultDeps() has nil functions: %+v", d)
	}
}

func TestSQLDBAdapter(t *testing.T) {
	// sql.Open does not dial; BeginTx with a cancelled context fails before
	// any network I/O, which exercises the adapter's error path.
	db, err := sql.Open("mysql", "u@tcp(127.0.0.1:1)/db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tx, err := (&sqlDB{db: db}).BeginTx(ctx, nil)
	if err == nil {
		t.Fatal("BeginTx with a cancelled context succeeded")
	}
	if tx != nil {
		t.Errorf("tx = %#v, want a nil interface on error", tx)
	}
}

func TestRootIDHexConversion(t *testing.T) {
	mixed := writeTemp(t, "mixed.json", `{"format_version": 1, "database": "x", "tables": [
	  {"name": "t", "columns": [
	    {"name": "id", "type": "int", "nullable": false},
	    {"name": "uuid", "type": "VARBINARY(4)", "nullable": false},
	    {"name": "label", "type": "varchar(10)", "nullable": false}],
	   "primary_key": ["id", "uuid", "label"]}], "foreign_keys": []}`)
	tests := []struct {
		name   string
		schema string
		table  string
		ids    []string
		want   []collect.Tuple
	}{
		{"binary PK hex", goldenSchema, "devices", []string{"0x0102", "0XAbCd"}, []collect.Tuple{{[]byte{1, 2}}, {[]byte{0xab, 0xcd}}}},
		{"binary PK empty hex", goldenSchema, "devices", []string{"0x"}, []collect.Tuple{{[]byte{}}}},
		{"binary PK without prefix stays a string", goldenSchema, "devices", []string{"abc", "x0x01"}, []collect.Tuple{{"abc"}, {"x0x01"}}},
		{"non-binary PK keeps 0x as a string", goldenSchema, "users", []string{"0x01"}, []collect.Tuple{{"0x01"}}},
		{"dedupe after conversion", goldenSchema, "devices", []string{"0x6162", "ab", "0X6162", "ab"}, []collect.Tuple{{[]byte("ab")}, {"ab"}}},
		{"composite mixing binary and non-binary", mixed, "t", []string{"0x01,0x01,0x01", `"0x01",0X,0x`, "0x01,0x01,0x01"},
			[]collect.Tuple{{"0x01", []byte{1}, "0x01"}, {"0x01", []byte{}, "0x"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"--schema", tt.schema, "--table", tt.table}
			for _, id := range tt.ids {
				args = append(args, "--id", id)
			}
			var errb bytes.Buffer
			inv, code := parseInvocation(args, IO{Out: &bytes.Buffer{}, Err: &errb})
			if code != proceed {
				t.Fatalf("code = %d; stderr:\n%s", code, errb.String())
			}
			if !reflect.DeepEqual(inv.ids, tt.want) {
				t.Errorf("ids = %#v, want %#v", inv.ids, tt.want)
			}
		})
	}

	t.Run("bad hex in composite names the column", func(t *testing.T) {
		code, _, stderr, opened := runCLI(t, "--schema", mixed, "--table", "t", "--id", "0xzz,0x1,a")
		if code != 2 || opened != 0 {
			t.Errorf("code = %d, opened = %d; want 2, 0", code, opened)
		}
		want := `relation-deleter: --id "0xzz,0x1,a": column "uuid": invalid hex value "0x1": odd length` + "\n"
		if stderr != want {
			t.Errorf("stderr = %q, want %q", stderr, want)
		}
	})
}
