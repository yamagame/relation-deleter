package report

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/yamagame/relation-deleter/internal/collect"
	"github.com/yamagame/relation-deleter/internal/execute"
	"github.com/yamagame/relation-deleter/internal/graph"
	"github.com/yamagame/relation-deleter/internal/plan"
	"github.com/yamagame/relation-deleter/internal/relations"
	"github.com/yamagame/relation-deleter/internal/schema"
)

// ---- fixture ----

func tbl(name string, pk []string, cols ...string) schema.Table {
	t := schema.Table{Name: name, PrimaryKey: pk}
	for _, c := range cols {
		t.Columns = append(t.Columns, schema.Column{Name: c, Type: "varchar(10)"})
	}
	return t
}

func fk(name, table, col, ref, refCol string) schema.ForeignKey {
	return schema.ForeignKey{Name: name, Table: table, Columns: []string{col}, ReferencedTable: ref, ReferencedColumns: []string{refCol}, DeleteRule: "RESTRICT"}
}

func manual(name, child, col, parent, pcol string) relations.ManualRelation {
	return relations.ManualRelation{
		Name:   name,
		Child:  relations.Endpoint{Table: child, Columns: []string{col}},
		Parent: relations.Endpoint{Table: parent, Columns: []string{pcol}},
	}
}

func edgeID(t *testing.T, g *graph.Graph, child, col string) int {
	t.Helper()
	for _, e := range g.Edges() {
		if e.ChildTable == child && slices.Equal(e.ChildColumns, []string{col}) {
			return e.ID
		}
	}
	t.Fatalf("edge %s(%s) not found", child, col)
	return -1
}

type row struct {
	key   collect.RowKey
	tuple collect.Tuple
	count int64
}

func set(table string, keyed bool, rows []row, via map[int][]collect.Tuple) *collect.TableSet {
	ts := &collect.TableSet{Table: table, Keyed: keyed, Rows: map[collect.RowKey]*collect.RowEntry{}, Via: map[int]*collect.ViaEdge{}}
	for _, r := range rows {
		n := r.count
		if n == 0 {
			n = 1
		}
		ts.Rows[r.key] = &collect.RowEntry{Key: r.tuple, Count: n}
	}
	for id, vals := range via {
		ts.Via[id] = &collect.ViaEdge{EdgeID: id, Values: vals}
	}
	return ts
}

func tup(vs ...any) collect.Tuple { return collect.Tuple(vs) }

func stmt(sql string, args ...any) plan.Statement { return plan.Statement{SQL: sql, Args: args} }

// fixture builds a graph, a collection and a plan by hand. It covers a single
// PK, a composite PK, a PK-less table reached by two edges, a cyclic group,
// []byte keys, a string with a quote and a manual relation source.
func fixture(t *testing.T) (*plan.Plan, *collect.Collection, *graph.Graph) {
	t.Helper()
	s := &schema.Schema{
		FormatVersion: 1,
		Database:      "app",
		Tables: []schema.Table{
			tbl("users", []string{"id"}, "id", "name"),
			tbl("orders", []string{"id"}, "id", "user_id"),
			tbl("order_items", []string{"order_id", "line_no"}, "order_id", "line_no"),
			tbl("employees", []string{"id"}, "id", "user_id", "manager_id"),
			tbl("audit_logs", nil, "user_id", "actor_name", "msg"),
			tbl("sessions", []string{"token"}, "token", "user_id"),
		},
		ForeignKeys: []schema.ForeignKey{
			fk("fk_orders_user", "orders", "user_id", "users", "id"),
			fk("fk_items_order", "order_items", "order_id", "orders", "id"),
			fk("fk_emp_user", "employees", "user_id", "users", "id"),
			fk("fk_emp_manager", "employees", "manager_id", "employees", "id"),
			fk("fk_audit_user", "audit_logs", "user_id", "users", "id"),
			fk("fk_sessions_user", "sessions", "user_id", "users", "id"),
		},
	}
	g := graph.Build(s, []relations.ManualRelation{
		manual("audit_actor", "audit_logs", "actor_name", "users", "name"),
		manual("orders_legacy", "orders", "user_id", "users", "id"),
	})

	users := set("users", true, []row{{"u1", tup("1"), 1}}, nil)
	users.Root = true
	c := &collect.Collection{Tables: map[string]*collect.TableSet{
		"users": users,
		"orders": set("orders", true,
			[]row{{"o2", tup("11"), 1}, {"o1", tup("10"), 1}},
			map[int][]collect.Tuple{edgeID(t, g, "orders", "user_id"): nil}),
		"order_items": set("order_items", true,
			// RowKey order (a, b, c) differs from the value order on purpose.
			[]row{{"b", tup("10", "1"), 1}, {"c", tup("10", "2"), 1}, {"a", tup("11", "1"), 1}},
			map[int][]collect.Tuple{edgeID(t, g, "order_items", "order_id"): nil}),
		"employees": set("employees", true,
			[]row{{"e1", tup("100"), 1}, {"e2", tup("101"), 1}},
			map[int][]collect.Tuple{edgeID(t, g, "employees", "user_id"): nil, edgeID(t, g, "employees", "manager_id"): nil}),
		"audit_logs": set("audit_logs", false,
			[]row{{"l1", tup("1", "O'Brien", "login"), 2}, {"l2", tup(nil, "O'Brien", "x"), 1}},
			map[int][]collect.Tuple{
				edgeID(t, g, "audit_logs", "user_id"):    {tup("1")},
				edgeID(t, g, "audit_logs", "actor_name"): {tup("O'Brien")},
			}),
		"sessions": set("sessions", true,
			[]row{{"s2", tup([]byte{0xde, 0xad}), 1}, {"s1", tup([]byte{0x00, 0x01}), 1}},
			map[int][]collect.Tuple{edgeID(t, g, "sessions", "user_id"): nil}),
	}, Total: 13}

	p := &plan.Plan{
		Groups: []plan.Group{
			{Steps: []plan.Step{{Table: "order_items", Expected: 3, Statements: []plan.Statement{
				stmt("DELETE FROM `order_items` WHERE (`order_id`,`line_no`) IN ((?,?),(?,?),(?,?))", "11", "1", "10", "1", "10", "2"),
			}}}},
			{Steps: []plan.Step{{Table: "orders", Expected: 2, Statements: []plan.Statement{
				stmt("DELETE FROM `orders` WHERE `id` IN (?,?)", "10", "11"),
			}}}},
			{Cyclic: true, Steps: []plan.Step{{Table: "employees", Expected: 2, Statements: []plan.Statement{
				stmt("DELETE FROM `employees` WHERE `id` IN (?)", "100"),
				stmt("DELETE FROM `employees` WHERE `id` IN (?)", "101"),
			}}}},
			{Steps: []plan.Step{{Table: "audit_logs", Expected: 3, Statements: []plan.Statement{
				stmt("DELETE FROM `audit_logs` WHERE `actor_name` IN (?)", "O'Brien"),
				stmt("DELETE FROM `audit_logs` WHERE `user_id` IN (?)", "1"),
			}}}},
			{Steps: []plan.Step{{Table: "sessions", Expected: 2, Statements: []plan.Statement{
				stmt("DELETE FROM `sessions` WHERE `token` IN (?,?)", []byte{0x00, 0x01}, []byte{0xde, 0xad}),
			}}}},
			{Steps: []plan.Step{{Table: "users", Expected: 1, Statements: []plan.Statement{
				stmt("DELETE FROM `users` WHERE `id` IN (?)", "1"),
			}}}},
		},
		Total:      13,
		TableCount: 6,
	}
	return p, c, g
}

// ---- RenderPlan ----

const wantPlan = `Deletion plan (child -> parent):
1. order_items: 3 rows
   via FK fk_items_order: order_items(order_id) -> orders(id)
   key ('11','1')
   key ('10','1')
   key ('10','2')
2. orders: 2 rows
   via FK fk_orders_user, manual orders_legacy: orders(user_id) -> users(id)
   key '10'
   key '11'
3. employees: 2 rows [cyclic group 3]
   via FK fk_emp_manager: employees(manager_id) -> employees(id)
   via FK fk_emp_user: employees(user_id) -> users(id)
   key '100'
   key '101'
4. audit_logs: 3 rows (no primary key)
   via manual audit_actor: audit_logs(actor_name) -> users(name)
   via FK fk_audit_user: audit_logs(user_id) -> users(id)
   match actor_name IN ('O\'Brien')
   match user_id IN ('1')
5. sessions: 2 rows
   via FK fk_sessions_user: sessions(user_id) -> users(id)
   key 0x0001
   key 0xdead
6. users: 1 row
   root
   key '1'

Statements (execution order):
DELETE FROM ` + "`order_items` WHERE (`order_id`,`line_no`) IN (('11','1'),('10','1'),('10','2'))" + `;
DELETE FROM ` + "`orders` WHERE `id` IN ('10','11')" + `;
SET SESSION foreign_key_checks = 0;
DELETE FROM ` + "`employees` WHERE `id` IN ('100')" + `;
DELETE FROM ` + "`employees` WHERE `id` IN ('101')" + `;
SET SESSION foreign_key_checks = 1;
DELETE FROM ` + "`audit_logs` WHERE `actor_name` IN ('O\\'Brien')" + `;
DELETE FROM ` + "`audit_logs` WHERE `user_id` IN ('1')" + `;
DELETE FROM ` + "`sessions` WHERE `token` IN (0x0001,0xdead)" + `;
DELETE FROM ` + "`users` WHERE `id` IN ('1')" + `;

total=13 tables=6
`

func TestRenderPlan(t *testing.T) {
	p, c, g := fixture(t)
	var b bytes.Buffer
	if err := RenderPlan(&b, p, c, g); err != nil {
		t.Fatalf("RenderPlan: %v", err)
	}
	if got := b.String(); got != wantPlan {
		t.Errorf("RenderPlan mismatch\n--- got ---\n%s\n--- want ---\n%s", got, wantPlan)
	}
}

func TestRenderPlanDeterministic(t *testing.T) {
	p, c, g := fixture(t)
	var first string
	for i := range 20 {
		var b bytes.Buffer
		if err := RenderPlan(&b, p, c, g); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = b.String()
		} else if b.String() != first {
			t.Fatalf("output differs on run %d", i)
		}
	}
}

func TestRenderPlanCompositeMatch(t *testing.T) {
	s := &schema.Schema{FormatVersion: 1, Tables: []schema.Table{
		tbl("p", []string{"a", "b"}, "a", "b"),
		tbl("c", nil, "x", "y"),
	}, ForeignKeys: []schema.ForeignKey{{Name: "fk_c_p", Table: "c", Columns: []string{"x", "y"}, ReferencedTable: "p", ReferencedColumns: []string{"a", "b"}}}}
	g := graph.Build(s, nil)
	id := g.Edges()[0].ID
	c := &collect.Collection{Tables: map[string]*collect.TableSet{
		"c": set("c", false, []row{{"r", tup("1", "2"), 4}}, map[int][]collect.Tuple{id: {tup("1", "2"), tup("3", "4")}}),
	}, Total: 4}
	p := &plan.Plan{Groups: []plan.Group{{Steps: []plan.Step{{Table: "c", Expected: 4, Statements: []plan.Statement{
		stmt("DELETE FROM `c` WHERE (`x`,`y`) IN ((?,?),(?,?))", "1", "2", "3", "4"),
	}}}}}, Total: 4, TableCount: 1}
	var b bytes.Buffer
	if err := RenderPlan(&b, p, c, g); err != nil {
		t.Fatal(err)
	}
	if want := "   match (x,y) IN (('1','2'),('3','4'))\n"; !strings.Contains(b.String(), want) {
		t.Errorf("missing %q in\n%s", want, b.String())
	}
}

func TestRenderPlanErrors(t *testing.T) {
	t.Run("table missing from collection", func(t *testing.T) {
		p, c, g := fixture(t)
		delete(c.Tables, "orders")
		if err := RenderPlan(&bytes.Buffer{}, p, c, g); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("unknown edge", func(t *testing.T) {
		p, c, g := fixture(t)
		c.Tables["orders"].Via[999] = &collect.ViaEdge{EdgeID: 999}
		if err := RenderPlan(&bytes.Buffer{}, p, c, g); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("unsupported arg type", func(t *testing.T) {
		p, c, g := fixture(t)
		p.Groups[1].Steps[0].Statements[0].Args[0] = 10
		if err := RenderPlan(&bytes.Buffer{}, p, c, g); !errors.Is(err, ErrLiteralType) {
			t.Fatalf("err = %v, want ErrLiteralType", err)
		}
	})
	t.Run("arg count mismatch", func(t *testing.T) {
		p, c, g := fixture(t)
		st := &p.Groups[1].Steps[0].Statements[0]
		st.Args = st.Args[:1]
		if err := RenderPlan(&bytes.Buffer{}, p, c, g); !errors.Is(err, ErrArgCount) {
			t.Fatalf("err = %v, want ErrArgCount", err)
		}
	})
	t.Run("write error", func(t *testing.T) {
		p, c, g := fixture(t)
		if err := RenderPlan(failWriter{}, p, c, g); err == nil {
			t.Fatal("want error")
		}
	})
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

// ---- summary, result, missing ----

func TestRenderSummary(t *testing.T) {
	p, _, _ := fixture(t)
	var b bytes.Buffer
	if err := RenderSummary(&b, p); err != nil {
		t.Fatal(err)
	}
	want := `Rows to delete:
  order_items: 3
  orders: 2
  employees: 2
  audit_logs: 3
  sessions: 2
  users: 1
total=13 tables=6
`
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
	if err := RenderSummary(failWriter{}, p); err == nil {
		t.Error("want write error")
	}
}

func TestRenderResult(t *testing.T) {
	rs := []execute.TableResult{
		{Table: "orders", Expected: 2, Deleted: 2},
		{Table: "users", Expected: 1, Deleted: 1},
	}
	var b bytes.Buffer
	if err := RenderResult(&b, rs); err != nil {
		t.Fatal(err)
	}
	want := `Deleted rows:
  orders: 2
  users: 1
total=3 tables=2
`
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
	if err := RenderResult(failWriter{}, rs); err == nil {
		t.Error("want write error")
	}
}

func TestRenderResultEmpty(t *testing.T) {
	var b bytes.Buffer
	if err := RenderResult(&b, nil); err != nil {
		t.Fatal(err)
	}
	if want := "Deleted rows:\ntotal=0 tables=0\n"; b.String() != want {
		t.Errorf("got %q want %q", b.String(), want)
	}
}

func TestRenderMissing(t *testing.T) {
	var b bytes.Buffer
	err := RenderMissing(&b, []collect.Tuple{tup("42"), tup("7", "it's"), tup([]byte{0xff})})
	if err != nil {
		t.Fatal(err)
	}
	want := `warning: root record not found: '42'
warning: root record not found: ('7','it\'s')
warning: root record not found: 0xff
`
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}

	b.Reset()
	if err := RenderMissing(&b, nil); err != nil || b.Len() != 0 {
		t.Errorf("empty: err=%v out=%q", err, b.String())
	}
	if err := RenderMissing(&bytes.Buffer{}, []collect.Tuple{tup(1)}); !errors.Is(err, ErrLiteralType) {
		t.Errorf("err = %v, want ErrLiteralType", err)
	}
}

// ---- literal ----

func TestLiteral(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"abc", "'abc'"},
		{"", "''"},
		{"O'Brien", `'O\'Brien'`},
		{`a\b`, `'a\\b'`},
		{"a\x00b", `'a\0b'`},
		{"l1\nl2\r", `'l1\nl2\r'`},
		{"z\x1a", `'z\Z'`},
		{"日本", "'日本'"},
		{[]byte{0x00, 0xab, 0xff}, "0x00abff"},
		{[]byte{}, "X''"},
		{nil, "NULL"},
	}
	for _, tc := range cases {
		got, err := literal(tc.in)
		if err != nil {
			t.Errorf("literal(%#v): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("literal(%#v) = %s, want %s", tc.in, got, tc.want)
		}
	}
	for _, bad := range []any{1, int64(2), 1.5, true, struct{}{}} {
		if _, err := literal(bad); !errors.Is(err, ErrLiteralType) {
			t.Errorf("literal(%#v) err = %v, want ErrLiteralType", bad, err)
		}
	}
}

func TestExpandSQL(t *testing.T) {
	cases := []struct {
		sql  string
		args []any
		want string
	}{
		{"DELETE FROM `t` WHERE `id` IN (?,?)", []any{"1", "2"}, "DELETE FROM `t` WHERE `id` IN ('1','2')"},
		{"DELETE FROM `wh?t` WHERE `c?l` IN (?)", []any{"x"}, "DELETE FROM `wh?t` WHERE `c?l` IN ('x')"},
		{"DELETE FROM `a``?` WHERE `b` IN (?)", []any{"x"}, "DELETE FROM `a``?` WHERE `b` IN ('x')"},
		{"DELETE FROM `t` WHERE `b` IN (?)", []any{"?"}, "DELETE FROM `t` WHERE `b` IN ('?')"},
		{"SELECT ?", []any{[]byte{1}}, "SELECT 0x01"},
		{"SET SESSION foreign_key_checks = 0", nil, "SET SESSION foreign_key_checks = 0"},
	}
	for _, tc := range cases {
		got, err := expandSQL(tc.sql, tc.args)
		if err != nil {
			t.Errorf("expandSQL(%q): %v", tc.sql, err)
			continue
		}
		if got != tc.want {
			t.Errorf("expandSQL(%q) = %q, want %q", tc.sql, got, tc.want)
		}
	}

	mismatch := []struct {
		sql  string
		args []any
	}{
		{"IN (?,?)", []any{"1"}},
		{"IN (?)", []any{"1", "2"}},
		{"`id?` IN (x)", []any{"1"}},
		{"`unterminated ? IN (?)", []any{"1"}},
	}
	for _, tc := range mismatch {
		if _, err := expandSQL(tc.sql, tc.args); !errors.Is(err, ErrArgCount) {
			t.Errorf("expandSQL(%q, %d args) err = %v, want ErrArgCount", tc.sql, len(tc.args), err)
		}
	}
	if _, err := expandSQL("IN (?)", []any{3}); !errors.Is(err, ErrLiteralType) {
		t.Errorf("err = %v, want ErrLiteralType", err)
	}
}

// ---- progress ----

func TestProgressThrottle(t *testing.T) {
	var b bytes.Buffer
	p := &Progress{W: &b}
	for _, n := range []int64{1, 500, 1000, 1500, 2100, 2999} {
		p.Collect("orders", n)
	}
	p.Collect("users", 3)
	p.Collect("users", 999)
	p.Collect("orders", 3000) // table changed back
	p.Collect("orders", 3001)
	p.Delete("orders", 500) // phase changed
	p.Delete("orders", 1000)
	p.Delete("users", 1)

	want := `collect orders: 1 rows
collect orders: 1000 rows
collect orders: 2100 rows
collect users: 3 rows
collect orders: 3000 rows
delete orders: 500 rows
delete orders: 1000 rows
delete users: 1 rows
`
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
}

func TestProgressCallbackTypes(t *testing.T) {
	p := &Progress{W: &bytes.Buffer{}}
	var _ collect.ProgressFunc = p.Collect
	e := execute.Executor{Progress: p.Delete}
	_ = e
}

func TestProgressNilWriter(t *testing.T) {
	p := &Progress{}
	p.Collect("t", 1) // must not panic
	p.Delete("t", 1000)
	var nilP *Progress
	nilP.Collect("t", 1)
}
