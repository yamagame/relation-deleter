package collect

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/yamagame/relation-deleter/internal/graph"
	"github.com/yamagame/relation-deleter/internal/schema"
)

// ---- fixtures -------------------------------------------------------------

func tbl(name string, pk []string, cols ...string) schema.Table {
	t := schema.Table{Name: name, PrimaryKey: pk}
	for _, c := range cols {
		t.Columns = append(t.Columns, schema.Column{Name: c, Type: "varchar(32)"})
	}
	return t
}

func fk(name, table, col, ref, refCol string) schema.ForeignKey {
	return schema.ForeignKey{Name: name, Table: table, Columns: []string{col}, ReferencedTable: ref, ReferencedColumns: []string{refCol}, DeleteRule: "RESTRICT"}
}

func pk(c ...string) []string { return c }

func testSchema() *schema.Schema {
	return &schema.Schema{
		FormatVersion: 1,
		Database:      "app",
		Tables: []schema.Table{
			tbl("users", pk("id"), "id", "email"),
			tbl("orders", pk("id"), "id", "user_id"),
			tbl("items", pk("id"), "id", "order_id"),
			tbl("notes", pk("id"), "id", "user_id", "order_id"),
			tbl("profiles", pk("id"), "id", "user_email"),
			tbl("profile_tags", pk("id"), "id", "profile_id"),
			tbl("categories", pk("id"), "id", "parent_id"),
			tbl("a", pk("id"), "id", "b_id"),
			tbl("b", pk("id"), "id", "a_id"),
			tbl("accounts", pk("id"), "id"),
			tbl("audit", nil, "account_id", "msg"),
			tbl("loose", nil, "x"),
		},
		ForeignKeys: []schema.ForeignKey{
			fk("fk_orders_user", "orders", "user_id", "users", "id"),
			fk("fk_items_order", "items", "order_id", "orders", "id"),
			fk("fk_notes_user", "notes", "user_id", "users", "id"),
			fk("fk_notes_order", "notes", "order_id", "orders", "id"),
			fk("fk_profiles_email", "profiles", "user_email", "users", "email"),
			fk("fk_tags_profile", "profile_tags", "profile_id", "profiles", "id"),
			fk("fk_categories_parent", "categories", "parent_id", "categories", "id"),
			fk("fk_a_b", "a", "b_id", "b", "id"),
			fk("fk_b_a", "b", "a_id", "a", "id"),
			fk("fk_audit_account", "audit", "account_id", "accounts", "id"),
		},
	}
}

type rec map[string]Value

func testData() map[string][]rec {
	return map[string][]rec{
		"users": {
			{"id": "1", "email": "u1@x"},
			{"id": "2", "email": "u2@x"},
			{"id": "3", "email": nil},
		},
		"orders": {
			{"id": "10", "user_id": "1"},
			{"id": "11", "user_id": "1"},
			{"id": "20", "user_id": "2"},
		},
		"items": {
			{"id": "100", "order_id": "10"},
			{"id": "101", "order_id": "11"},
			{"id": "200", "order_id": "20"},
			{"id": "999", "order_id": nil},
		},
		"notes": {
			// reached both via notes.user_id and via notes.order_id
			{"id": "n1", "user_id": "1", "order_id": "10"},
			{"id": "n2", "user_id": nil, "order_id": "20"},
		},
		"profiles": {
			{"id": "p1", "user_email": "u1@x"},
			{"id": "p2", "user_email": "u2@x"},
			{"id": "p0", "user_email": nil},
		},
		"profile_tags": {
			{"id": "t1", "profile_id": "p1"},
			{"id": "t0", "profile_id": "p0"},
		},
		"categories": {
			{"id": "c1", "parent_id": nil},
			{"id": "c2", "parent_id": "c1"},
			{"id": "c3", "parent_id": "c2"},
			{"id": "c4", "parent_id": "c2"},
			{"id": "c9", "parent_id": nil},
		},
		"a": {
			{"id": "a1", "b_id": "b1"},
			{"id": "a2", "b_id": "b2"},
		},
		"b": {
			{"id": "b1", "a_id": "a1"},
			{"id": "b2", "a_id": "a2"},
		},
		"accounts": {{"id": "1"}},
		"audit":    {{"account_id": "1", "msg": "hi"}},
	}
}

// ---- fake RowSource ---------------------------------------------------------

type call struct {
	Table   string
	Columns []string
	Keyed   bool
	Pred    Predicate
}

// fakeSource is a deterministic in-memory RowSource. It only implements
// Fetch, which is the whole RowSource interface; it records every call.
type fakeSource struct {
	data  map[string][]rec
	calls []call
	err   map[string]error // table -> error to return
	after func(n int)      // called after each Fetch with the call count
	chunk int              // predicate tuples per chunk; 0 means one chunk
}

func eq(a, b Value) bool {
	if a == nil || b == nil {
		return false // NULL never matches
	}
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []byte:
		y, ok := b.([]byte)
		return ok && bytes.Equal(x, y)
	}
	return false
}

func (f *fakeSource) Fetch(ctx context.Context, table string, columns []string, keyed bool, p Predicate) ([]Row, error) {
	f.calls = append(f.calls, call{table, slices.Clone(columns), keyed, p})
	defer func() {
		if f.after != nil {
			f.after(len(f.calls))
		}
	}()
	if err := f.err[table]; err != nil {
		return nil, err
	}
	// Like the SQL implementation, the predicate is split into chunks and the
	// results are concatenated; with keyed=false each chunk is grouped by all
	// requested columns, so one tuple can appear once per chunk.
	size := f.chunk
	if size <= 0 {
		size = len(p.Values)
	}
	var out []Row
	for chunk := range slices.Chunk(p.Values, max(size, 1)) {
		rows, err := f.fetchChunk(table, columns, keyed, Predicate{Columns: p.Columns, Values: chunk})
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

func (f *fakeSource) fetchChunk(table string, columns []string, keyed bool, p Predicate) ([]Row, error) {
	var out []Row
	group := map[RowKey]int{} // keyed=false: tuple -> index in out
	for _, r := range f.data[table] {
		match := false
		for _, tup := range p.Values {
			all := true
			for i, c := range p.Columns {
				if !eq(r[c], tup[i]) {
					all = false
					break
				}
			}
			if all {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		vals := make(Tuple, len(columns))
		for i, c := range columns {
			vals[i] = r[c]
		}
		if !keyed {
			k, err := keyOf(vals)
			if err != nil {
				return nil, err
			}
			if i, ok := group[k]; ok {
				out[i].Count++
				continue
			}
			group[k] = len(out)
		}
		out = append(out, Row{Values: vals, Count: 1})
	}
	return out, nil
}

// ---- helpers ------------------------------------------------------------------

func newCollector(src RowSource) (*Collector, *graph.Graph) {
	s := testSchema()
	g := graph.Build(s, nil)
	return &Collector{Graph: g, Schema: s, Source: src}, g
}

func ids(v ...string) []Tuple {
	out := make([]Tuple, len(v))
	for i, s := range v {
		out[i] = Tuple{s}
	}
	return out
}

// keys returns the sorted first PK value of each row of a table.
func keys(c *Collection, table string) []string {
	ts := c.Tables[table]
	if ts == nil {
		return nil
	}
	var out []string
	for _, e := range ts.Rows {
		out = append(out, e.Key[0].(string))
	}
	sort.Strings(out)
	return out
}

func tableNames(c *Collection) []string {
	var out []string
	for n := range c.Tables {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func edgeID(t *testing.T, g *graph.Graph, child, childCol, parent string) int {
	t.Helper()
	for _, e := range g.Edges() {
		if e.ChildTable == child && e.ChildColumns[0] == childCol && e.ParentTable == parent {
			return e.ID
		}
	}
	t.Fatalf("edge %s.%s -> %s not found", child, childCol, parent)
	return -1
}

func viaIDs(ts *TableSet) []int {
	var out []int
	for id, v := range ts.Via {
		if v.EdgeID != id {
			panic("Via key and EdgeID differ")
		}
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

// ---- tests ----------------------------------------------------------------------

func TestCollectMultiLevel(t *testing.T) {
	c, g := newCollector(&fakeSource{data: testData()})
	got, err := c.Collect(context.Background(), "users", ids("1"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"users":        {"1"},
		"orders":       {"10", "11"},
		"items":        {"100", "101"},
		"notes":        {"n1"},
		"profiles":     {"p1"}, // reached through the non-PK column users.email (4.5)
		"profile_tags": {"t1"}, // grandchild through the non-PK edge
	}
	for table, w := range want {
		if k := keys(got, table); !reflect.DeepEqual(k, w) {
			t.Errorf("%s: got %v, want %v", table, k, w)
		}
	}
	if names := tableNames(got); len(names) != len(want) {
		t.Errorf("tables = %v, want only %d tables", names, len(want))
	}
	if got.Total != 8 {
		t.Errorf("Total = %d, want 8", got.Total)
	}
	if len(got.MissingRoots) != 0 {
		t.Errorf("MissingRoots = %v, want none", got.MissingRoots)
	}
	if !got.Tables["users"].Root || got.Tables["orders"].Root {
		t.Errorf("Root flags wrong: users=%v orders=%v", got.Tables["users"].Root, got.Tables["orders"].Root)
	}
	for _, ts := range got.Tables {
		if !ts.Keyed {
			t.Errorf("%s: Keyed = false", ts.Table)
		}
		for _, e := range ts.Rows {
			if e.Count != 1 {
				t.Errorf("%s: Count = %d", ts.Table, e.Count)
			}
		}
	}
	// RowEntry.Values holds the PK and the referenced columns.
	u := got.Tables["users"].Rows[mustKey(t, Tuple{"1"})]
	if u == nil || !reflect.DeepEqual(u.Values, map[string]Value{"id": "1", "email": "u1@x"}) {
		t.Errorf("users row values = %+v", u)
	}
	if len(got.Tables["items"].Via) != 1 || got.Tables["items"].Via[edgeID(t, g, "items", "order_id", "orders")] == nil {
		t.Errorf("items Via = %v", viaIDs(got.Tables["items"]))
	}
}

func mustKey(t *testing.T, tup Tuple) RowKey {
	t.Helper()
	k, err := keyOf(tup)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestCollectFetchColumns(t *testing.T) {
	src := &fakeSource{data: testData()}
	c, _ := newCollector(src)
	if _, err := c.Collect(context.Background(), "users", ids("1")); err != nil {
		t.Fatal(err)
	}
	first := src.calls[0]
	if first.Table != "users" || !first.Keyed ||
		!reflect.DeepEqual(first.Columns, []string{"id", "email"}) ||
		!reflect.DeepEqual(first.Pred, Predicate{Columns: []string{"id"}, Values: []Tuple{{"1"}}}) {
		t.Errorf("root call = %+v", first)
	}
	for _, cl := range src.calls {
		if !cl.Keyed {
			t.Errorf("%s fetched with keyed=false", cl.Table)
		}
		if cl.Table == "profiles" {
			if !reflect.DeepEqual(cl.Pred, Predicate{Columns: []string{"user_email"}, Values: []Tuple{{"u1@x"}}}) {
				t.Errorf("profiles predicate = %+v", cl.Pred)
			}
		}
		if cl.Table == "items" && !reflect.DeepEqual(cl.Columns, []string{"id"}) {
			t.Errorf("items columns = %v", cl.Columns)
		}
		for _, tup := range cl.Pred.Values {
			if slices.Contains(tup, nil) {
				t.Errorf("%s predicate contains NULL: %v", cl.Table, cl.Pred)
			}
		}
		if len(cl.Pred.Values) == 0 {
			t.Errorf("%s fetched with an empty predicate", cl.Table)
		}
	}
}

func TestCollectCycles(t *testing.T) {
	t.Run("mutual reference terminates and counts once (4.3)", func(t *testing.T) {
		src := &fakeSource{data: testData()}
		c, _ := newCollector(src)
		got, err := c.Collect(context.Background(), "a", ids("a1"))
		if err != nil {
			t.Fatal(err)
		}
		if k := keys(got, "a"); !reflect.DeepEqual(k, []string{"a1"}) {
			t.Errorf("a = %v", k)
		}
		if k := keys(got, "b"); !reflect.DeepEqual(k, []string{"b1"}) {
			t.Errorf("b = %v", k)
		}
		if got.Total != 2 {
			t.Errorf("Total = %d, want 2", got.Total)
		}
		// a1 is reached again through b.a_id -> a.id, so a records that edge too.
		if len(got.Tables["a"].Via) != 1 {
			t.Errorf("a Via = %v", viaIDs(got.Tables["a"]))
		}
		if len(src.calls) > 4 {
			t.Errorf("too many fetches: %d", len(src.calls))
		}
	})
	t.Run("self-reference tree terminates (4.3)", func(t *testing.T) {
		c, _ := newCollector(&fakeSource{data: testData()})
		got, err := c.Collect(context.Background(), "categories", ids("c1", "c2"))
		if err != nil {
			t.Fatal(err)
		}
		if k := keys(got, "categories"); !reflect.DeepEqual(k, []string{"c1", "c2", "c3", "c4"}) {
			t.Errorf("categories = %v", k)
		}
		if got.Total != 4 {
			t.Errorf("Total = %d, want 4", got.Total)
		}
		if len(got.Tables) != 1 || !got.Tables["categories"].Root {
			t.Errorf("tables = %v", tableNames(got))
		}
	})
}

func TestCollectNullNotFollowed(t *testing.T) {
	src := &fakeSource{data: testData()}
	c, _ := newCollector(src)
	// users.3 has email NULL: profiles p0 (user_email NULL) must not be reached.
	got, err := c.Collect(context.Background(), "users", ids("3"))
	if err != nil {
		t.Fatal(err)
	}
	if names := tableNames(got); !reflect.DeepEqual(names, []string{"users"}) {
		t.Errorf("tables = %v, want only users", names)
	}
	for _, cl := range src.calls {
		if cl.Table == "profiles" {
			t.Errorf("profiles fetched with %+v although the only email is NULL", cl.Pred)
		}
	}
	if got.Total != 1 {
		t.Errorf("Total = %d", got.Total)
	}
}

func TestCollectMissingRoots(t *testing.T) {
	t.Run("partial", func(t *testing.T) {
		src := &fakeSource{data: testData()}
		c, _ := newCollector(src)
		got, err := c.Collect(context.Background(), "users", ids("404", "2", "1", "405"))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.MissingRoots, ids("404", "405")) {
			t.Errorf("MissingRoots = %v", got.MissingRoots)
		}
		if k := keys(got, "users"); !reflect.DeepEqual(k, []string{"1", "2"}) {
			t.Errorf("users = %v", k)
		}
		roots := 0
		for _, cl := range src.calls {
			if cl.Table == "users" {
				roots++
				if len(cl.Pred.Values) != 1 {
					t.Errorf("root fetch with %d tuples, want one per call", len(cl.Pred.Values))
				}
			}
		}
		if roots != 4 {
			t.Errorf("root fetches = %d, want 4", roots)
		}
	})
	t.Run("all missing", func(t *testing.T) {
		c, _ := newCollector(&fakeSource{data: testData()})
		got, err := c.Collect(context.Background(), "users", ids("404", "405"))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.MissingRoots, ids("404", "405")) {
			t.Errorf("MissingRoots = %v", got.MissingRoots)
		}
		if got.Tables == nil || len(got.Tables) != 0 || got.Total != 0 {
			t.Errorf("Tables = %v Total = %d, want empty", tableNames(got), got.Total)
		}
	})
	t.Run("duplicate ids count once", func(t *testing.T) {
		c, _ := newCollector(&fakeSource{data: testData()})
		got, err := c.Collect(context.Background(), "categories", ids("c9", "c9"))
		if err != nil {
			t.Fatal(err)
		}
		if got.Total != 1 || len(got.MissingRoots) != 0 {
			t.Errorf("Total = %d Missing = %v", got.Total, got.MissingRoots)
		}
	})
}

func TestCollectViaRecordsEveryEdge(t *testing.T) {
	c, g := newCollector(&fakeSource{data: testData()})
	got, err := c.Collect(context.Background(), "users", ids("1"))
	if err != nil {
		t.Fatal(err)
	}
	// n1 is reached via notes.user_id first and again via notes.order_id,
	// where it is already collected; both edges must be recorded (5.5).
	want := []int{edgeID(t, g, "notes", "order_id", "orders"), edgeID(t, g, "notes", "user_id", "users")}
	sort.Ints(want)
	if v := viaIDs(got.Tables["notes"]); !reflect.DeepEqual(v, want) {
		t.Errorf("notes Via = %v, want %v", v, want)
	}
	if len(got.Tables["notes"].Rows) != 1 {
		t.Errorf("notes rows = %d, want 1", len(got.Tables["notes"].Rows))
	}
	if len(got.Tables["users"].Via) != 0 {
		t.Errorf("root users Via = %v, want none", viaIDs(got.Tables["users"]))
	}
}

func TestCollectProgress(t *testing.T) {
	type ev struct {
		table string
		n     int64
	}
	var evs []ev
	src := &fakeSource{data: testData()}
	c, _ := newCollector(src)
	c.Progress = func(table string, fetched int64) { evs = append(evs, ev{table, fetched}) }
	if _, err := c.Collect(context.Background(), "users", ids("1", "2")); err != nil {
		t.Fatal(err)
	}
	if len(evs) != len(src.calls) {
		t.Fatalf("progress called %d times, want once per Fetch (%d)", len(evs), len(src.calls))
	}
	if evs[0] != (ev{"users", 1}) || evs[1] != (ev{"users", 2}) {
		t.Errorf("root progress = %v, want cumulative users 1, 2", evs[:2])
	}
	last := map[string]int64{}
	for _, e := range evs {
		if e.n < last[e.table] {
			t.Errorf("progress for %s decreased: %d after %d", e.table, e.n, last[e.table])
		}
		last[e.table] = e.n
	}
	// notes: n1 via user_id (1 row), then n1+n2 via order_id (2 rows) -> cumulative 3.
	if last["notes"] != 3 {
		t.Errorf("notes cumulative = %d, want 3 (events %v)", last["notes"], evs)
	}
	if last["orders"] != 3 {
		t.Errorf("orders cumulative = %d, want 3", last["orders"])
	}
}

func TestCollectErrors(t *testing.T) {
	t.Run("source error wrapped with table name", func(t *testing.T) {
		boom := errors.New("boom")
		c, _ := newCollector(&fakeSource{data: testData(), err: map[string]error{"items": boom}})
		_, err := c.Collect(context.Background(), "users", ids("1"))
		if !errors.Is(err, boom) || !strings.Contains(err.Error(), "items") {
			t.Errorf("err = %v, want wrapped boom naming items", err)
		}
	})
	t.Run("root source error wrapped", func(t *testing.T) {
		boom := errors.New("boom")
		c, _ := newCollector(&fakeSource{data: testData(), err: map[string]error{"users": boom}})
		_, err := c.Collect(context.Background(), "users", ids("1"))
		if !errors.Is(err, boom) || !strings.Contains(err.Error(), "users") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("unkeyed root", func(t *testing.T) {
		c, _ := newCollector(&fakeSource{data: testData()})
		_, err := c.Collect(context.Background(), "loose", ids("1"))
		if err == nil || !strings.Contains(err.Error(), "loose") || !strings.Contains(err.Error(), "primary key") {
			t.Errorf("err = %v, want an error naming loose and its missing primary key", err)
		}
	})
	t.Run("unknown root table", func(t *testing.T) {
		c, _ := newCollector(&fakeSource{data: testData()})
		if _, err := c.Collect(context.Background(), "nope", ids("1")); err == nil {
			t.Error("want error for an unknown table")
		}
	})
	t.Run("root id arity mismatch", func(t *testing.T) {
		c, _ := newCollector(&fakeSource{data: testData()})
		if _, err := c.Collect(context.Background(), "users", []Tuple{{"1", "2"}}); err == nil {
			t.Error("want error for a root id with the wrong number of values")
		}
	})
	t.Run("context cancelled before start", func(t *testing.T) {
		src := &fakeSource{data: testData()}
		c, _ := newCollector(src)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := c.Collect(ctx, "users", ids("1"))
		if !errors.Is(err, context.Canceled) || len(src.calls) != 0 {
			t.Errorf("err = %v calls = %d", err, len(src.calls))
		}
	})
	t.Run("context cancelled mid traversal", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		src := &fakeSource{data: testData(), after: func(n int) {
			if n == 2 {
				cancel()
			}
		}}
		c, _ := newCollector(src)
		_, err := c.Collect(ctx, "users", ids("1"))
		if !errors.Is(err, context.Canceled) || len(src.calls) != 2 {
			t.Errorf("err = %v calls = %d, want Canceled after 2 fetches", err, len(src.calls))
		}
	})
}

func TestRowSourceIsReadOnly(t *testing.T) {
	typ := reflect.TypeFor[RowSource]()
	if typ.NumMethod() != 1 || typ.Method(0).Name != "Fetch" {
		t.Errorf("RowSource must expose only Fetch (4.6), has %d methods", typ.NumMethod())
	}
}

func TestKeyOfInjective(t *testing.T) {
	tuples := []Tuple{
		{nil}, {""}, {[]byte{}}, {"a|b"}, {"a", "b"}, {"a|", "b"}, {[]byte("a")}, {"a"},
		{nil, "a"}, {"", "a"}, {"\x00"}, {"s\x01a"}, {}, {nil, nil},
	}
	seen := map[RowKey]Tuple{}
	for _, tup := range tuples {
		k := mustKey(t, tup)
		if prev, ok := seen[k]; ok {
			t.Errorf("%#v and %#v share RowKey %q", prev, tup, k)
		}
		seen[k] = tup
	}
	if _, err := keyOf(Tuple{7}); !errors.Is(err, ErrValueType) {
		t.Errorf("int value: err = %v, want ErrValueType", err)
	}
}

func TestCollectBinaryValues(t *testing.T) {
	s := &schema.Schema{
		FormatVersion: 1,
		Tables: []schema.Table{
			{Name: "p", PrimaryKey: pk("id"), Columns: []schema.Column{{Name: "id", Type: "binary(2)"}}},
			{Name: "c", PrimaryKey: pk("id"), Columns: []schema.Column{{Name: "id", Type: "int"}, {Name: "pid", Type: "binary(2)"}}},
		},
		ForeignKeys: []schema.ForeignKey{fk("fk_c_p", "c", "pid", "p", "id")},
	}
	src := &fakeSource{data: map[string][]rec{
		"p": {{"id": []byte{1, 2}}, {"id": []byte{3, 4}}},
		"c": {{"id": "1", "pid": []byte{1, 2}}, {"id": "2", "pid": []byte{3, 4}}},
	}}
	c := &Collector{Graph: graph.Build(s, nil), Schema: s, Source: src}
	got, err := c.Collect(context.Background(), "p", []Tuple{{[]byte{1, 2}}})
	if err != nil {
		t.Fatal(err)
	}
	if k := keys(got, "c"); !reflect.DeepEqual(k, []string{"1"}) {
		t.Errorf("c = %v", k)
	}
}
