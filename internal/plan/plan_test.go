package plan

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/yamagame/mysql-relation-deleter/internal/collect"
	"github.com/yamagame/mysql-relation-deleter/internal/graph"
	"github.com/yamagame/mysql-relation-deleter/internal/schema"
)

// ---- helpers ----

func tbl(name string, pk []string, cols ...string) schema.Table {
	t := schema.Table{Name: name, PrimaryKey: pk}
	for _, c := range cols {
		t.Columns = append(t.Columns, schema.Column{Name: c, Type: "varchar(10)"})
	}
	return t
}

func fk(name, table string, cols []string, ref string, refCols []string) schema.ForeignKey {
	return schema.ForeignKey{Name: name, Table: table, Columns: cols, ReferencedTable: ref, ReferencedColumns: refCols, DeleteRule: "RESTRICT"}
}

func sch(tables []schema.Table, fks ...schema.ForeignKey) *schema.Schema {
	return &schema.Schema{FormatVersion: 1, Database: "app", Tables: tables, ForeignKeys: fks}
}

func edgeID(t *testing.T, g *graph.Graph, child string, childCols []string, parent string) int {
	t.Helper()
	for _, e := range g.Edges() {
		if e.ChildTable == child && slices.Equal(e.ChildColumns, childCols) && e.ParentTable == parent {
			return e.ID
		}
	}
	t.Fatalf("edge %s%v -> %s not found", child, childCols, parent)
	return -1
}

// keyed builds a TableSet of a PK table. Each key is a tuple; RowKeys are
// synthesized from the given order labels so tests control sort order.
func keyed(table string, rows map[collect.RowKey]collect.Tuple) *collect.TableSet {
	ts := &collect.TableSet{Table: table, Keyed: true, Rows: map[collect.RowKey]*collect.RowEntry{}, Via: map[int]*collect.ViaEdge{}}
	for k, tup := range rows {
		ts.Rows[k] = &collect.RowEntry{Key: tup, Count: 1}
	}
	return ts
}

type unkeyedRow struct {
	key   collect.RowKey
	tuple collect.Tuple
	count int64
}

func unkeyed(table string, rows []unkeyedRow, via map[int][]collect.Tuple) *collect.TableSet {
	ts := &collect.TableSet{Table: table, Rows: map[collect.RowKey]*collect.RowEntry{}, Via: map[int]*collect.ViaEdge{}}
	for _, r := range rows {
		ts.Rows[r.key] = &collect.RowEntry{Key: r.tuple, Count: r.count}
	}
	for id, vals := range via {
		ts.Via[id] = &collect.ViaEdge{EdgeID: id, Values: vals}
	}
	return ts
}

func coll(sets ...*collect.TableSet) *collect.Collection {
	c := &collect.Collection{Tables: map[string]*collect.TableSet{}}
	for _, ts := range sets {
		c.Tables[ts.Table] = ts
		for _, r := range ts.Rows {
			c.Total += r.Count
		}
	}
	return c
}

func tup(vs ...any) collect.Tuple { return collect.Tuple(vs) }

// ---- SQL shapes ----

func TestBuildSQLShapes(t *testing.T) {
	// users(id) <- user_roles(user_id,role_id) composite PK
	// users(id) <- logs(user_id) PK-less, one edge
	// user_roles(user_id,role_id) <- audit(ua,ra) and audit(ub,rb), PK-less, two edges
	s := sch([]schema.Table{
		tbl("users", []string{"id"}, "id"),
		tbl("user_roles", []string{"user_id", "role_id"}, "user_id", "role_id"),
		tbl("logs", nil, "user_id", "msg"),
		tbl("audit", nil, "ua", "ra", "ub", "rb"),
	},
		fk("fk_ur", "user_roles", []string{"user_id"}, "users", []string{"id"}),
		fk("fk_logs", "logs", []string{"user_id"}, "users", []string{"id"}),
		fk("fk_audit_a", "audit", []string{"ua", "ra"}, "user_roles", []string{"user_id", "role_id"}),
		fk("fk_audit_b", "audit", []string{"ub", "rb"}, "user_roles", []string{"user_id", "role_id"}),
	)
	g := graph.Build(s, nil)
	eLogs := edgeID(t, g, "logs", []string{"user_id"}, "users")
	eA := edgeID(t, g, "audit", []string{"ua", "ra"}, "user_roles")
	eB := edgeID(t, g, "audit", []string{"ub", "rb"}, "user_roles")
	if eA > eB {
		t.Fatalf("test assumes edge A (%d) < edge B (%d)", eA, eB)
	}

	c := coll(
		keyed("users", map[collect.RowKey]collect.Tuple{"k2": tup("2"), "k1": tup("1")}),
		keyed("user_roles", map[collect.RowKey]collect.Tuple{
			"k1": tup("1", "10"), "k2": tup("2", "20"),
		}),
		unkeyed("logs", []unkeyedRow{
			{"r1", tup("1", "hello"), 3},
			{"r2", tup("2", "bye"), 1},
		}, map[int][]collect.Tuple{eLogs: {tup("2"), tup("1")}}),
		unkeyed("audit", []unkeyedRow{
			{"r1", tup("1", "10", "2", "20"), 2},
			{"r2", tup("2", "20", "1", "10"), 1},
		}, map[int][]collect.Tuple{
			eB: {tup("2", "20"), tup("1", "10")},
			eA: {tup("1", "10"), tup("2", "20")},
		}),
	)

	p, err := Build(c, g, s, Options{})
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]Step{
		"users": {Table: "users", Expected: 2, Statements: []Statement{
			{SQL: "DELETE FROM `users` WHERE `id` IN (?,?)", Args: []any{"1", "2"}},
		}},
		"user_roles": {Table: "user_roles", Expected: 2, Statements: []Statement{
			{SQL: "DELETE FROM `user_roles` WHERE (`user_id`,`role_id`) IN ((?,?),(?,?))", Args: []any{"1", "10", "2", "20"}},
		}},
		"logs": {Table: "logs", Expected: 4, Statements: []Statement{
			{SQL: "DELETE FROM `logs` WHERE `user_id` IN (?,?)", Args: []any{"2", "1"}},
		}},
		"audit": {Table: "audit", Expected: 3, Statements: []Statement{
			{SQL: "DELETE FROM `audit` WHERE (`ua`,`ra`) IN ((?,?),(?,?))", Args: []any{"1", "10", "2", "20"}},
			{SQL: "DELETE FROM `audit` WHERE (`ub`,`rb`) IN ((?,?),(?,?))", Args: []any{"2", "20", "1", "10"}},
		}},
	}
	got := map[string]Step{}
	var order []string
	for _, gr := range p.Groups {
		if gr.Cyclic {
			t.Errorf("group %v is cyclic", gr)
		}
		for _, st := range gr.Steps {
			got[st.Table] = st
			order = append(order, st.Table)
		}
	}
	for name, w := range want {
		if !reflect.DeepEqual(got[name], w) {
			t.Errorf("step %s:\n got %#v\nwant %#v", name, got[name], w)
		}
	}
	// child -> parent: audit before user_roles, user_roles and logs before users.
	wantOrder := []string{"audit", "logs", "user_roles", "users"}
	if !slices.Equal(order, wantOrder) {
		t.Errorf("order = %v, want %v", order, wantOrder)
	}
	if p.Total != c.Total || p.Total != 11 {
		t.Errorf("Total = %d, collection %d, want 11", p.Total, c.Total)
	}
	if p.TableCount != 4 {
		t.Errorf("TableCount = %d, want 4", p.TableCount)
	}
}

// ---- chunking ----

func TestEffectiveChunkSize(t *testing.T) {
	tests := []struct {
		chunk, ncols, want int
	}{
		{0, 1, 500},
		{-3, 1, 500},
		{2, 1, 2},
		{500, 2, 500},
		{30000, 3, 21845},
		{70000, 1, 65535},
		{500, 200, 327},
		{10, 70000, 1},
	}
	for _, tt := range tests {
		if got := EffectiveChunkSize(tt.chunk, tt.ncols); got != tt.want {
			t.Errorf("EffectiveChunkSize(%d,%d) = %d, want %d", tt.chunk, tt.ncols, got, tt.want)
		}
	}
}

func TestBuildChunking(t *testing.T) {
	s := sch([]schema.Table{tbl("users", []string{"id"}, "id")})
	g := graph.Build(s, nil)
	rows := map[collect.RowKey]collect.Tuple{}
	for _, k := range []string{"e", "c", "a", "d", "b"} {
		rows[collect.RowKey(k)] = tup(k)
	}
	c := coll(keyed("users", rows))
	p, err := Build(c, g, s, Options{ChunkSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	want := []Statement{
		{SQL: "DELETE FROM `users` WHERE `id` IN (?,?)", Args: []any{"a", "b"}},
		{SQL: "DELETE FROM `users` WHERE `id` IN (?,?)", Args: []any{"c", "d"}},
		{SQL: "DELETE FROM `users` WHERE `id` IN (?)", Args: []any{"e"}},
	}
	if got := p.Groups[0].Steps[0].Statements; !reflect.DeepEqual(got, want) {
		t.Errorf("statements:\n got %#v\nwant %#v", got, want)
	}
	if p.Groups[0].Steps[0].Expected != 5 {
		t.Errorf("Expected = %d, want 5", p.Groups[0].Steps[0].Expected)
	}
}

func TestBuildChunkingUnkeyedComposite(t *testing.T) {
	s := sch([]schema.Table{
		tbl("p", []string{"a", "b"}, "a", "b"),
		tbl("c", nil, "pa", "pb"),
	}, fk("fk", "c", []string{"pa", "pb"}, "p", []string{"a", "b"}))
	g := graph.Build(s, nil)
	e := edgeID(t, g, "c", []string{"pa", "pb"}, "p")
	c := coll(unkeyed("c", []unkeyedRow{{"r", tup("1", "1"), 3}},
		map[int][]collect.Tuple{e: {tup("1", "1"), tup("2", "2"), tup("3", "3")}}))
	p, err := Build(c, g, s, Options{ChunkSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	want := []Statement{
		{SQL: "DELETE FROM `c` WHERE (`pa`,`pb`) IN ((?,?),(?,?))", Args: []any{"1", "1", "2", "2"}},
		{SQL: "DELETE FROM `c` WHERE (`pa`,`pb`) IN ((?,?))", Args: []any{"3", "3"}},
	}
	if got := p.Groups[0].Steps[0].Statements; !reflect.DeepEqual(got, want) {
		t.Errorf("statements:\n got %#v\nwant %#v", got, want)
	}
}

// TestBuildLargeStaysUnderPlaceholderLimit checks 8.1: a huge ChunkSize is
// shrunk so no statement exceeds 65,535 placeholders.
func TestBuildLargeStaysUnderPlaceholderLimit(t *testing.T) {
	s := sch([]schema.Table{tbl("t", []string{"a", "b", "c"}, "a", "b", "c")})
	g := graph.Build(s, nil)
	rows := map[collect.RowKey]collect.Tuple{}
	const n = 50000
	for i := range n {
		k := string(rune(0x10000 + i))
		rows[collect.RowKey(k)] = tup(k, "x", "y")
	}
	c := coll(keyed("t", rows))
	p, err := Build(c, g, s, Options{ChunkSize: 30000})
	if err != nil {
		t.Fatal(err)
	}
	stmts := p.Groups[0].Steps[0].Statements
	if len(stmts) != 3 { // 21845 + 21845 + 6310
		t.Fatalf("len(statements) = %d, want 3", len(stmts))
	}
	total := 0
	for _, st := range stmts {
		if len(st.Args) > MaxPlaceholders {
			t.Errorf("statement has %d args", len(st.Args))
		}
		if strings.Count(st.SQL, "?") != len(st.Args) {
			t.Errorf("placeholder count %d != args %d", strings.Count(st.SQL, "?"), len(st.Args))
		}
		total += len(st.Args)
	}
	if len(stmts[0].Args) != 21845*3 || total != n*3 {
		t.Errorf("first chunk args = %d, total = %d", len(stmts[0].Args), total)
	}
}

// ---- order and cyclic groups ----

func TestBuildGroupsAndCyclic(t *testing.T) {
	// nodes self-references; a <-> b is a mutual pair; leaf references a.
	s := sch([]schema.Table{
		tbl("nodes", []string{"id"}, "id", "parent_id"),
		tbl("a", []string{"id"}, "id", "b_id"),
		tbl("b", []string{"id"}, "id", "a_id"),
		tbl("leaf", []string{"id"}, "id", "a_id"),
	},
		fk("fk_nodes", "nodes", []string{"parent_id"}, "nodes", []string{"id"}),
		fk("fk_ab", "a", []string{"b_id"}, "b", []string{"id"}),
		fk("fk_ba", "b", []string{"a_id"}, "a", []string{"id"}),
		fk("fk_leaf", "leaf", []string{"a_id"}, "a", []string{"id"}),
	)
	g := graph.Build(s, nil)
	c := coll(
		keyed("nodes", map[collect.RowKey]collect.Tuple{"1": tup("1"), "2": tup("2")}),
		keyed("a", map[collect.RowKey]collect.Tuple{"1": tup("1")}),
		keyed("b", map[collect.RowKey]collect.Tuple{"1": tup("1")}),
		keyed("leaf", map[collect.RowKey]collect.Tuple{"1": tup("1")}),
	)
	p, err := Build(c, g, s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	dg := g.DeleteOrder([]string{"nodes", "a", "b", "leaf"})
	if len(p.Groups) != len(dg) {
		t.Fatalf("groups = %d, want %d", len(p.Groups), len(dg))
	}
	type shape struct {
		Cyclic bool
		Tables []string
	}
	var got, want []shape
	for i, gr := range p.Groups {
		var names []string
		for _, st := range gr.Steps {
			names = append(names, st.Table)
		}
		got = append(got, shape{gr.Cyclic, names})
		want = append(want, shape{dg[i].Cyclic, dg[i].Tables})
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("groups:\n got %v\nwant %v", got, want)
	}
	// Explicitly: leaf (non-cyclic) before the {a,b} cyclic group; nodes cyclic.
	wantExplicit := []shape{{false, []string{"leaf"}}, {true, []string{"a", "b"}}, {true, []string{"nodes"}}}
	if !reflect.DeepEqual(got, wantExplicit) {
		t.Errorf("groups:\n got %v\nwant %v", got, wantExplicit)
	}
	if p.Total != 5 || p.TableCount != 4 {
		t.Errorf("Total = %d, TableCount = %d", p.Total, p.TableCount)
	}
}

// ---- determinism, quoting, bytes ----

func TestBuildDeterministic(t *testing.T) {
	s := sch([]schema.Table{
		tbl("p", []string{"id"}, "id"),
		tbl("c", nil, "pid"),
	}, fk("fk", "c", []string{"pid"}, "p", []string{"id"}))
	g := graph.Build(s, nil)
	e := edgeID(t, g, "c", []string{"pid"}, "p")
	mk := func() *collect.Collection {
		rows := map[collect.RowKey]collect.Tuple{}
		for _, k := range []string{"z", "m", "a", "q", "b", "x", "c"} {
			rows[collect.RowKey(k)] = tup(k)
		}
		return coll(keyed("p", rows),
			unkeyed("c", []unkeyedRow{{"r", tup("z"), 2}}, map[int][]collect.Tuple{e: {tup("z"), tup("a")}}))
	}
	p1, err := Build(mk(), g, s, Options{ChunkSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		p2, err := Build(mk(), g, s, Options{ChunkSize: 3})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(p1, p2) {
			t.Fatalf("plans differ:\n%#v\n%#v", p1, p2)
		}
	}
}

func TestBuildQuotesIdentifiers(t *testing.T) {
	s := sch([]schema.Table{
		tbl("we`ird", []string{"i`d"}, "i`d"),
		tbl("ch`ild", nil, "p`id"),
	}, fk("fk", "ch`ild", []string{"p`id"}, "we`ird", []string{"i`d"}))
	g := graph.Build(s, nil)
	e := edgeID(t, g, "ch`ild", []string{"p`id"}, "we`ird")
	c := coll(
		keyed("we`ird", map[collect.RowKey]collect.Tuple{"1": tup("1")}),
		unkeyed("ch`ild", []unkeyedRow{{"r", tup("1"), 1}}, map[int][]collect.Tuple{e: {tup("1")}}),
	)
	p, err := Build(c, g, s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var sqls []string
	for _, gr := range p.Groups {
		for _, st := range gr.Steps {
			for _, x := range st.Statements {
				sqls = append(sqls, x.SQL)
			}
		}
	}
	want := []string{
		"DELETE FROM `ch``ild` WHERE `p``id` IN (?)",
		"DELETE FROM `we``ird` WHERE `i``d` IN (?)",
	}
	if !slices.Equal(sqls, want) {
		t.Errorf("sql = %q, want %q", sqls, want)
	}
}

func TestBuildPreservesBytes(t *testing.T) {
	s := sch([]schema.Table{
		tbl("p", []string{"id"}, "id"),
		tbl("c", nil, "pid"),
	}, fk("fk", "c", []string{"pid"}, "p", []string{"id"}))
	g := graph.Build(s, nil)
	e := edgeID(t, g, "c", []string{"pid"}, "p")
	b1, b2 := []byte{0x00, 0xff}, []byte{0x01}
	c := coll(
		keyed("p", map[collect.RowKey]collect.Tuple{"a": tup(b1), "b": tup(b2)}),
		unkeyed("c", []unkeyedRow{{"r", tup(b1), 1}}, map[int][]collect.Tuple{e: {tup(b1)}}),
	)
	p, err := Build(c, g, s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, gr := range p.Groups {
		for _, st := range gr.Steps {
			var want []any
			if st.Table == "p" {
				want = []any{b1, b2}
			} else {
				want = []any{b1}
			}
			got := st.Statements[0].Args
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s args = %#v, want %#v", st.Table, got, want)
			}
			for _, a := range got {
				if _, ok := a.([]byte); !ok {
					t.Errorf("%s arg %#v is %T, want []byte", st.Table, a, a)
				}
			}
		}
	}
}

// ---- errors ----

func TestBuildErrors(t *testing.T) {
	s := sch([]schema.Table{
		tbl("p", []string{"a", "b"}, "a", "b"),
		tbl("c", nil, "pa", "pb"),
	}, fk("fk", "c", []string{"pa", "pb"}, "p", []string{"a", "b"}))
	g := graph.Build(s, nil)
	e := edgeID(t, g, "c", []string{"pa", "pb"}, "p")
	pRows := func() *collect.TableSet {
		return keyed("p", map[collect.RowKey]collect.Tuple{"1": tup("1", "1")})
	}

	tests := []struct {
		name string
		c    *collect.Collection
		want error
	}{
		{"table missing from schema", coll(keyed("ghost", map[collect.RowKey]collect.Tuple{"1": tup("1")})), ErrUnknownTable},
		{"key length mismatch", coll(keyed("p", map[collect.RowKey]collect.Tuple{"1": tup("1")})), ErrKeyLength},
		{"pk-less without via", coll(pRows(), unkeyed("c", []unkeyedRow{{"r", tup("1", "1"), 1}}, nil)), ErrNoVia},
		{"pk-less with empty via values", coll(pRows(), unkeyed("c", []unkeyedRow{{"r", tup("1", "1"), 1}},
			map[int][]collect.Tuple{e: nil})), ErrNoVia},
		{"nil value in via", coll(pRows(), unkeyed("c", []unkeyedRow{{"r", tup("1", "1"), 1}},
			map[int][]collect.Tuple{e: {tup("1", nil)}})), ErrNullValue},
		{"via tuple length mismatch", coll(pRows(), unkeyed("c", []unkeyedRow{{"r", tup("1", "1"), 1}},
			map[int][]collect.Tuple{e: {tup("1")}})), ErrKeyLength},
		{"unknown via edge", coll(pRows(), unkeyed("c", []unkeyedRow{{"r", tup("1", "1"), 1}},
			map[int][]collect.Tuple{999: {tup("1", "1")}})), ErrBadEdge},
		{"nil value in keyed key", coll(keyed("p", map[collect.RowKey]collect.Tuple{"1": tup("1", nil)})), ErrNullValue},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Build(tt.c, g, s, Options{})
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if p != nil {
				t.Errorf("plan = %#v, want nil", p)
			}
		})
	}

	t.Run("total mismatch", func(t *testing.T) {
		c := coll(pRows())
		c.Total = 99
		_, err := Build(c, g, s, Options{})
		if !errors.Is(err, ErrTotalMismatch) {
			t.Fatalf("err = %v, want %v", err, ErrTotalMismatch)
		}
	})
}

func TestBuildEmpty(t *testing.T) {
	s := sch([]schema.Table{tbl("p", []string{"id"}, "id")})
	g := graph.Build(s, nil)
	p, err := Build(&collect.Collection{Tables: map[string]*collect.TableSet{}}, g, s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Groups) != 0 || p.Total != 0 || p.TableCount != 0 {
		t.Errorf("plan = %#v", p)
	}
}
