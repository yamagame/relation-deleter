package collect

import (
	"context"
	"reflect"
	"testing"

	"github.com/yamagame/relation-deleter/internal/graph"
	"github.com/yamagame/relation-deleter/internal/relations"
	"github.com/yamagame/relation-deleter/internal/schema"
)

func fkN(name, table string, cols []string, ref string, refCols []string) schema.ForeignKey {
	return schema.ForeignKey{Name: name, Table: table, Columns: cols, ReferencedTable: ref, ReferencedColumns: refCols, DeleteRule: "RESTRICT"}
}

func manual(name, child string, childCols []string, parent string, parentCols []string) relations.ManualRelation {
	return relations.ManualRelation{
		Name:   name,
		Child:  relations.Endpoint{Table: child, Columns: childCols},
		Parent: relations.Endpoint{Table: parent, Columns: parentCols},
	}
}

// findEdge returns the edge child(childCols) -> parent.
func findEdge(t *testing.T, g *graph.Graph, child string, childCols []string, parent string) *graph.Edge {
	t.Helper()
	for _, e := range g.Edges() {
		if e.ChildTable == child && reflect.DeepEqual(e.ChildColumns, childCols) && e.ParentTable == parent {
			return e
		}
	}
	t.Fatalf("edge %s%v -> %s not found", child, childCols, parent)
	return nil
}

func callsFor(src *fakeSource, table string) []call {
	var out []call
	for _, cl := range src.calls {
		if cl.Table == table {
			out = append(out, cl)
		}
	}
	return out
}

// unkeyedSchema: accounts(id) is referenced by the PK-less audit through two
// edges (account_id and actor_id). audit is itself the parent of
// audit_replies (manual relation on audit.ref) and of audit_marks (FK on the
// UNIQUE column audit.ref).
func unkeyedSchema() *schema.Schema {
	return &schema.Schema{
		FormatVersion: 1,
		Database:      "app",
		Tables: []schema.Table{
			tbl("accounts", pk("id"), "id"),
			tbl("audit", nil, "account_id", "actor_id", "msg", "ref"),
			tbl("audit_replies", pk("id"), "id", "audit_ref"),
			tbl("audit_marks", nil, "audit_ref", "mark"),
		},
		ForeignKeys: []schema.ForeignKey{
			fk("fk_audit_account", "audit", "account_id", "accounts", "id"),
			fk("fk_audit_actor", "audit", "actor_id", "accounts", "id"),
			fk("fk_marks_audit", "audit_marks", "audit_ref", "audit", "ref"),
		},
	}
}

func unkeyedData() map[string][]rec {
	return map[string][]rec{
		"accounts": {{"id": "1"}, {"id": "2"}, {"id": "3"}},
		"audit": {
			// two identical rows, matched by both edges for account 1
			{"account_id": "1", "actor_id": "1", "msg": "self", "ref": "r1"},
			{"account_id": "1", "actor_id": "1", "msg": "self", "ref": "r1"},
			{"account_id": "1", "actor_id": "3", "msg": "by3", "ref": nil},
			{"account_id": "3", "actor_id": "2", "msg": "on3", "ref": "r2"},
			{"account_id": "3", "actor_id": "3", "msg": "other", "ref": "r9"},
		},
		"audit_replies": {
			{"id": "x1", "audit_ref": "r1"},
			{"id": "x2", "audit_ref": "r2"},
			{"id": "x9", "audit_ref": "r9"},
		},
		"audit_marks": {
			{"audit_ref": "r2", "mark": "m"},
			{"audit_ref": "r2", "mark": "m"},
			{"audit_ref": "r2", "mark": "m"},
		},
	}
}

func newUnkeyedCollector(src *fakeSource) (*Collector, *graph.Graph) {
	s := unkeyedSchema()
	g := graph.Build(s, []relations.ManualRelation{
		manual("replies_audit", "audit_replies", []string{"audit_ref"}, "audit", []string{"ref"}),
	})
	return &Collector{Graph: g, Schema: s, Source: src}, g
}

func entry(t *testing.T, ts *TableSet, tup Tuple) *RowEntry {
	t.Helper()
	if ts == nil {
		t.Fatalf("table not collected")
	}
	return ts.Rows[mustKey(t, tup)]
}

func TestCollectUnkeyedChild(t *testing.T) {
	for _, chunk := range []int{0, 1} {
		name := "one chunk"
		if chunk == 1 {
			name = "one tuple per chunk"
		}
		t.Run(name, func(t *testing.T) {
			src := &fakeSource{data: unkeyedData(), chunk: chunk}
			c, g := newUnkeyedCollector(src)
			got, err := c.Collect(context.Background(), "accounts", ids("1", "2"))
			if err != nil {
				t.Fatal(err)
			}
			audit := got.Tables["audit"]
			if audit == nil || audit.Keyed {
				t.Fatalf("audit = %+v, want a PK-less TableSet", audit)
			}
			// (a) identical rows: one entry with Count 2 (4.4).
			self := entry(t, audit, Tuple{"1", "1", "self", "r1"})
			if self == nil || self.Count != 2 || !reflect.DeepEqual(self.Key, Tuple{"1", "1", "self", "r1"}) {
				t.Errorf("self entry = %+v, want full tuple key with Count 2", self)
			}
			if e := entry(t, audit, Tuple{"1", "3", "by3", nil}); e == nil || e.Count != 1 {
				t.Errorf("by3 entry = %+v", e)
			}
			if e := entry(t, audit, Tuple{"3", "2", "on3", "r2"}); e == nil || e.Count != 1 {
				t.Errorf("on3 entry = %+v", e)
			}
			if len(audit.Rows) != 3 {
				t.Errorf("audit rows = %d, want 3", len(audit.Rows))
			}
			wantValues := map[string]Value{"account_id": "1", "actor_id": "1", "msg": "self", "ref": "r1"}
			if self != nil && !reflect.DeepEqual(self.Values, wantValues) {
				t.Errorf("self Values = %v", self.Values)
			}

			// (b) reached via two edges: both recorded with their predicate values.
			acc := findEdge(t, g, "audit", []string{"account_id"}, "accounts")
			act := findEdge(t, g, "audit", []string{"actor_id"}, "accounts")
			if v := viaIDs(audit); len(v) != 2 {
				t.Fatalf("audit Via = %v, want both edges", v)
			}
			if v := audit.Via[acc.ID].Values; !reflect.DeepEqual(v, []Tuple{{"1"}, {"2"}}) {
				t.Errorf("Via[account_id].Values = %v, want [[1] [2]]", v)
			}
			if v := audit.Via[act.ID].Values; !reflect.DeepEqual(v, []Tuple{{"1"}, {"2"}}) {
				t.Errorf("Via[actor_id].Values = %v, want [[1] [2]]", v)
			}

			// (c) the PK-less audit is the parent of audit_replies (manual
			// relation) and audit_marks (FK on the UNIQUE column ref).
			if k := keys(got, "audit_replies"); !reflect.DeepEqual(k, []string{"x1", "x2"}) {
				t.Errorf("audit_replies = %v, want [x1 x2]", k)
			}
			marks := got.Tables["audit_marks"]
			if e := entry(t, marks, Tuple{"r2", "m"}); e == nil || e.Count != 3 {
				t.Errorf("audit_marks entry = %+v, want Count 3", e)
			}
			me := findEdge(t, g, "audit_marks", []string{"audit_ref"}, "audit")
			if v := marks.Via[me.ID].Values; !reflect.DeepEqual(v, []Tuple{{"r2"}}) {
				t.Errorf("audit_marks Via Values = %v, want [[r2]]", v)
			}

			// Each tuple counts once, by its Count: accounts 2 + audit 2+1+1
			// + replies 2 + marks 3.
			if got.Total != 11 {
				t.Errorf("Total = %d, want 11", got.Total)
			}
			var sum int64
			for _, ts := range got.Tables {
				for _, e := range ts.Rows {
					sum += e.Count
				}
			}
			if sum != got.Total {
				t.Errorf("sum of Counts = %d, Total = %d", sum, got.Total)
			}

			// Keyed tables leave Via Values empty.
			for id, v := range got.Tables["audit_replies"].Via {
				if v.Values != nil {
					t.Errorf("keyed audit_replies Via[%d].Values = %v, want nil", id, v.Values)
				}
			}
		})
	}
}

func TestCollectUnkeyedFetchArgs(t *testing.T) {
	src := &fakeSource{data: unkeyedData()}
	c, _ := newUnkeyedCollector(src)
	if _, err := c.Collect(context.Background(), "accounts", ids("1", "2")); err != nil {
		t.Fatal(err)
	}
	// (f) PK-less tables are fetched with keyed=false and all columns in
	// schema order; keyed tables with keyed=true.
	audits := callsFor(src, "audit")
	if len(audits) != 2 {
		t.Fatalf("audit fetched %d times, want once per edge", len(audits))
	}
	for _, cl := range audits {
		if cl.Keyed || !reflect.DeepEqual(cl.Columns, []string{"account_id", "actor_id", "msg", "ref"}) {
			t.Errorf("audit call = %+v, want keyed=false and all columns", cl)
		}
	}
	// audit_marks is followed once per BFS batch of audit rows: r1 (self; the
	// NULL ref of by3 is not followed), then r2 (on3).
	var markPreds []Predicate
	for _, cl := range callsFor(src, "audit_marks") {
		if cl.Keyed || !reflect.DeepEqual(cl.Columns, []string{"audit_ref", "mark"}) {
			t.Errorf("audit_marks call = %+v", cl)
		}
		markPreds = append(markPreds, cl.Pred)
	}
	wantPreds := []Predicate{
		{Columns: []string{"audit_ref"}, Values: []Tuple{{"r1"}}},
		{Columns: []string{"audit_ref"}, Values: []Tuple{{"r2"}}},
	}
	if !reflect.DeepEqual(markPreds, wantPreds) {
		t.Errorf("audit_marks predicates = %+v, want %+v", markPreds, wantPreds)
	}
	for _, table := range []string{"accounts", "audit_replies"} {
		for _, cl := range callsFor(src, table) {
			if !cl.Keyed {
				t.Errorf("%s fetched with keyed=false", table)
			}
		}
	}
}

func TestCollectUnkeyedProgress(t *testing.T) {
	src := &fakeSource{data: unkeyedData()}
	c, _ := newUnkeyedCollector(src)
	last := map[string]int64{}
	c.Progress = func(table string, n int64) { last[table] = n }
	if _, err := c.Collect(context.Background(), "accounts", ids("1", "2")); err != nil {
		t.Fatal(err)
	}
	// account_id IN (1,2): self(2) + by3(1); actor_id IN (1,2): self(2) + on3(1).
	if last["audit"] != 6 {
		t.Errorf("audit cumulative = %d, want 6 (sum of Counts fetched)", last["audit"])
	}
	if last["audit_marks"] != 3 {
		t.Errorf("audit_marks cumulative = %d, want 3", last["audit_marks"])
	}
}

func TestCollectUnkeyedViaAccumulatesAcrossFetches(t *testing.T) {
	// nodes is a self-referencing tree; logs is PK-less and references the
	// non-unique nodes.grp through a manual relation, so the same edge is
	// followed once per BFS level and the value "g" is sent twice.
	s := &schema.Schema{
		FormatVersion: 1,
		Tables: []schema.Table{
			tbl("nodes", pk("id"), "id", "parent_id", "grp"),
			tbl("logs", nil, "grp", "msg"),
		},
		ForeignKeys: []schema.ForeignKey{fk("fk_nodes_parent", "nodes", "parent_id", "nodes", "id")},
	}
	g := graph.Build(s, []relations.ManualRelation{manual("logs_grp", "logs", []string{"grp"}, "nodes", []string{"grp"})})
	src := &fakeSource{data: map[string][]rec{
		"nodes": {
			{"id": "1", "parent_id": nil, "grp": "g"},
			{"id": "2", "parent_id": "1", "grp": "h"},
			{"id": "3", "parent_id": "1", "grp": "g"},
		},
		"logs": {{"grp": "g", "msg": "a"}, {"grp": "h", "msg": "b"}, {"grp": "h", "msg": "b"}},
	}}
	c := &Collector{Graph: g, Schema: s, Source: src}
	got, err := c.Collect(context.Background(), "nodes", ids("1"))
	if err != nil {
		t.Fatal(err)
	}
	logs := got.Tables["logs"]
	e := findEdge(t, g, "logs", []string{"grp"}, "nodes")
	if v := logs.Via[e.ID].Values; !reflect.DeepEqual(v, []Tuple{{"g"}, {"h"}}) {
		t.Errorf("Via Values = %v, want deduplicated [[g] [h]] in fetch order", v)
	}
	if got.Total != 3+1+2 {
		t.Errorf("Total = %d, want 6", got.Total)
	}
	if n := len(callsFor(src, "logs")); n != 2 {
		t.Errorf("logs fetched %d times, want 2", n)
	}
}

// (d) 4.5: a keyed child is referenced through its non-PK UNIQUE column, so
// fetching it must include that column, and its grandchildren are collected.
func TestCollectNonPKReferenceFromChild(t *testing.T) {
	s := &schema.Schema{
		FormatVersion: 1,
		Tables: []schema.Table{
			tbl("users", pk("id"), "id"),
			tbl("orders", pk("id"), "id", "user_id", "code"),
			tbl("shipments", pk("id"), "id", "order_code"),
			tbl("parcels", pk("id"), "id", "shipment_id"),
		},
		ForeignKeys: []schema.ForeignKey{
			fk("fk_orders_user", "orders", "user_id", "users", "id"),
			fk("fk_ship_order", "shipments", "order_code", "orders", "code"),
			fk("fk_parcels_ship", "parcels", "shipment_id", "shipments", "id"),
		},
	}
	src := &fakeSource{data: map[string][]rec{
		"users":     {{"id": "1"}},
		"orders":    {{"id": "10", "user_id": "1", "code": "A"}, {"id": "11", "user_id": "1", "code": nil}, {"id": "20", "user_id": "2", "code": "B"}},
		"shipments": {{"id": "s1", "order_code": "A"}, {"id": "s2", "order_code": "B"}},
		"parcels":   {{"id": "q1", "shipment_id": "s1"}, {"id": "q2", "shipment_id": "s2"}},
	}}
	c := &Collector{Graph: graph.Build(s, nil), Schema: s, Source: src}
	got, err := c.Collect(context.Background(), "users", ids("1"))
	if err != nil {
		t.Fatal(err)
	}
	if k := keys(got, "shipments"); !reflect.DeepEqual(k, []string{"s1"}) {
		t.Errorf("shipments = %v", k)
	}
	if k := keys(got, "parcels"); !reflect.DeepEqual(k, []string{"q1"}) {
		t.Errorf("parcels = %v", k)
	}
	for _, cl := range callsFor(src, "orders") {
		if !reflect.DeepEqual(cl.Columns, []string{"id", "code"}) {
			t.Errorf("orders columns = %v, want PK plus the referenced code", cl.Columns)
		}
	}
	for _, cl := range callsFor(src, "shipments") {
		if !reflect.DeepEqual(cl.Pred, Predicate{Columns: []string{"order_code"}, Values: []Tuple{{"A"}}}) {
			t.Errorf("shipments predicate = %+v", cl.Pred)
		}
	}
	if got.Total != 5 {
		t.Errorf("Total = %d, want 5", got.Total)
	}
}

// (e) composite PKs and a 2-column edge whose ParentColumns order differs
// from the parent's PK order.
func TestCollectCompositeKeys(t *testing.T) {
	s := &schema.Schema{
		FormatVersion: 1,
		Tables: []schema.Table{
			tbl("members", pk("tenant", "uid"), "tenant", "uid", "name"),
			tbl("grants", pk("gid", "tenant"), "gid", "tenant", "m_uid", "m_tenant"),
			tbl("grant_logs", pk("id"), "id", "g_tenant", "g_gid"),
		},
		ForeignKeys: []schema.ForeignKey{
			fkN("fk_grants_member", "grants", []string{"m_uid", "m_tenant"}, "members", []string{"uid", "tenant"}),
			fkN("fk_logs_grant", "grant_logs", []string{"g_tenant", "g_gid"}, "grants", []string{"tenant", "gid"}),
		},
	}
	src := &fakeSource{data: map[string][]rec{
		"members": {
			{"tenant": "t1", "uid": "u1", "name": "a"},
			{"tenant": "t2", "uid": "u1", "name": "b"},
		},
		"grants": {
			{"gid": "g1", "tenant": "t1", "m_uid": "u1", "m_tenant": "t1"},
			{"gid": "g2", "tenant": "t1", "m_uid": "u1", "m_tenant": "t2"},
			{"gid": "g1", "tenant": "t2", "m_uid": "u1", "m_tenant": "t1"},
		},
		"grant_logs": {
			{"id": "l1", "g_tenant": "t1", "g_gid": "g1"},
			{"id": "l2", "g_tenant": "t2", "g_gid": "g1"},
			{"id": "l3", "g_tenant": "t1", "g_gid": "g2"},
		},
	}}
	g := graph.Build(s, nil)
	c := &Collector{Graph: g, Schema: s, Source: src}
	got, err := c.Collect(context.Background(), "members", []Tuple{{"t1", "u1"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tables["members"].Rows[mustKey(t, Tuple{"t1", "u1"})] == nil || len(got.Tables["members"].Rows) != 1 {
		t.Errorf("members rows = %v", got.Tables["members"].Rows)
	}
	grants := got.Tables["grants"]
	if len(grants.Rows) != 2 {
		t.Errorf("grants rows = %d, want 2", len(grants.Rows))
	}
	for _, k := range []Tuple{{"g1", "t1"}, {"g1", "t2"}} {
		e := entry(t, grants, k)
		if e == nil || !reflect.DeepEqual(e.Key, k) {
			t.Errorf("grants %v = %+v, want key in PK order", k, e)
		}
	}
	if k := keys(got, "grant_logs"); !reflect.DeepEqual(k, []string{"l1", "l2"}) {
		t.Errorf("grant_logs = %v, want [l1 l2]", k)
	}
	want := []call{
		{"members", []string{"tenant", "uid"}, true, Predicate{Columns: []string{"tenant", "uid"}, Values: []Tuple{{"t1", "u1"}}}},
		{"grants", []string{"gid", "tenant"}, true, Predicate{Columns: []string{"m_uid", "m_tenant"}, Values: []Tuple{{"u1", "t1"}}}},
		{"grant_logs", []string{"id"}, true, Predicate{Columns: []string{"g_tenant", "g_gid"}, Values: []Tuple{{"t1", "g1"}, {"t2", "g1"}}}},
	}
	// grants rows arrive in data order, so the grant_logs predicate is deterministic.
	if !reflect.DeepEqual(src.calls, want) {
		t.Errorf("calls =\n%+v\nwant\n%+v", src.calls, want)
	}
	if got.Total != 5 {
		t.Errorf("Total = %d, want 5", got.Total)
	}
}

// dupSource returns every grouped row twice for PK-less tables, as a SQL
// RowSource does when two chunks contain values that the column's collation
// treats as equal (for example "r" and "R"): each chunk reports all copies.
type dupSource struct{ fakeSource }

func (d *dupSource) Fetch(ctx context.Context, table string, columns []string, keyed bool, p Predicate) ([]Row, error) {
	rows, err := d.fakeSource.Fetch(ctx, table, columns, keyed, p)
	if keyed || err != nil {
		return rows, err
	}
	return append(rows, rows...), nil
}

func TestCollectUnkeyedRepeatedTupleCountsOnce(t *testing.T) {
	src := &dupSource{fakeSource{data: unkeyedData()}}
	s := unkeyedSchema()
	c := &Collector{Graph: graph.Build(s, nil), Schema: s, Source: src}
	got, err := c.Collect(context.Background(), "accounts", ids("1"))
	if err != nil {
		t.Fatal(err)
	}
	// account 1: self(2) + by3(1) via account_id, self(2) via actor_id.
	if e := entry(t, got.Tables["audit"], Tuple{"1", "1", "self", "r1"}); e == nil || e.Count != 2 {
		t.Errorf("self entry = %+v, want Count 2", e)
	}
	if got.Total != 1+2+1 {
		t.Errorf("Total = %d, want 4", got.Total)
	}
}

func TestCollectUnkeyedInvalidCount(t *testing.T) {
	src := &zeroCountSource{}
	s := unkeyedSchema()
	c := &Collector{Graph: graph.Build(s, nil), Schema: s, Source: src}
	if _, err := c.Collect(context.Background(), "accounts", ids("1")); err == nil {
		t.Error("want an error for a grouped row with Count 0")
	}
}

type zeroCountSource struct{}

func (zeroCountSource) Fetch(_ context.Context, table string, columns []string, keyed bool, p Predicate) ([]Row, error) {
	if keyed {
		return []Row{{Values: Tuple{"1"}, Count: 1}}, nil
	}
	return []Row{{Values: Tuple{"1", "1", "m", nil}, Count: 0}}, nil
}
