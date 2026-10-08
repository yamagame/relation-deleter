package graph

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yamagame/relation-deleter/internal/relations"
	"github.com/yamagame/relation-deleter/internal/schema"
)

func fk(name, table string, cols []string, ref string, refCols []string) schema.ForeignKey {
	return schema.ForeignKey{Name: name, Table: table, Columns: cols, ReferencedTable: ref, ReferencedColumns: refCols, DeleteRule: "CASCADE"}
}

func manual(name, child string, childCols []string, parent string, parentCols []string) relations.ManualRelation {
	return relations.ManualRelation{
		Name:   name,
		Child:  relations.Endpoint{Table: child, Columns: childCols},
		Parent: relations.Endpoint{Table: parent, Columns: parentCols},
	}
}

func cols(c ...string) []string { return c }

// edgeView is Edge without the pointer identity, for comparison.
type edgeView struct {
	ID            int
	ChildTable    string
	ChildColumns  []string
	ParentTable   string
	ParentColumns []string
	Sources       []EdgeSource
}

func view(edges []*Edge) []edgeView {
	out := make([]edgeView, 0, len(edges))
	for _, e := range edges {
		out = append(out, edgeView{e.ID, e.ChildTable, e.ChildColumns, e.ParentTable, e.ParentColumns, e.Sources})
	}
	return out
}

func baseSchema(fks ...schema.ForeignKey) *schema.Schema {
	return &schema.Schema{
		FormatVersion: 1,
		Database:      "app",
		Tables: []schema.Table{
			{Name: "users", Columns: []schema.Column{{Name: "id"}, {Name: "email"}}, PrimaryKey: cols("id")},
			{Name: "orders", Columns: []schema.Column{{Name: "id"}, {Name: "user_id"}}, PrimaryKey: cols("id")},
			{Name: "legacy", Columns: []schema.Column{{Name: "customer_id"}, {Name: "a"}, {Name: "b"}}},
			{Name: "pairs", Columns: []schema.Column{{Name: "a"}, {Name: "b"}}, PrimaryKey: cols("a", "b")},
		},
		ForeignKeys: fks,
	}
}

func TestBuild(t *testing.T) {
	fkOrders := fk("fk_orders_user", "orders", cols("user_id"), "users", cols("id"))
	tests := []struct {
		name   string
		fks    []schema.ForeignKey
		manual []relations.ManualRelation
		want   []edgeView
	}{
		{
			name: "no manual relations gives FK edges only (2.2)",
			fks:  []schema.ForeignKey{fkOrders},
			want: []edgeView{
				{0, "orders", cols("user_id"), "users", cols("id"), []EdgeSource{{SourceForeignKey, "fk_orders_user"}}},
			},
		},
		{
			name:   "manual relation adds an edge (2.1)",
			fks:    []schema.ForeignKey{fkOrders},
			manual: []relations.ManualRelation{manual("legacy_user", "legacy", cols("customer_id"), "users", cols("id"))},
			want: []edgeView{
				{0, "legacy", cols("customer_id"), "users", cols("id"), []EdgeSource{{SourceManual, "legacy_user"}}},
				{1, "orders", cols("user_id"), "users", cols("id"), []EdgeSource{{SourceForeignKey, "fk_orders_user"}}},
			},
		},
		{
			name:   "manual relation duplicating an FK is merged into one edge with two sources (2.5)",
			fks:    []schema.ForeignKey{fkOrders},
			manual: []relations.ManualRelation{manual("dup", "orders", cols("user_id"), "users", cols("id"))},
			want: []edgeView{
				{0, "orders", cols("user_id"), "users", cols("id"), []EdgeSource{{SourceForeignKey, "fk_orders_user"}, {SourceManual, "dup"}}},
			},
		},
		{
			name: "sources are sorted FK first then manual, each by name",
			fks: []schema.ForeignKey{
				fk("fk_z", "orders", cols("user_id"), "users", cols("id")),
				fk("fk_a", "orders", cols("user_id"), "users", cols("id")),
			},
			manual: []relations.ManualRelation{
				manual("m_z", "orders", cols("user_id"), "users", cols("id")),
				manual("m_a", "orders", cols("user_id"), "users", cols("id")),
			},
			want: []edgeView{
				{0, "orders", cols("user_id"), "users", cols("id"), []EdgeSource{
					{SourceForeignKey, "fk_a"}, {SourceForeignKey, "fk_z"}, {SourceManual, "m_a"}, {SourceManual, "m_z"},
				}},
			},
		},
		{
			name: "different column order is a different edge",
			manual: []relations.ManualRelation{
				manual("ab", "legacy", cols("a", "b"), "pairs", cols("a", "b")),
				manual("ba", "legacy", cols("b", "a"), "pairs", cols("b", "a")),
			},
			want: []edgeView{
				{0, "legacy", cols("a", "b"), "pairs", cols("a", "b"), []EdgeSource{{SourceManual, "ab"}}},
				{1, "legacy", cols("b", "a"), "pairs", cols("b", "a"), []EdgeSource{{SourceManual, "ba"}}},
			},
		},
		{
			name:   "unnamed manual relation gets manual#<i>",
			manual: []relations.ManualRelation{manual("x", "orders", cols("user_id"), "users", cols("id")), manual("", "legacy", cols("customer_id"), "users", cols("id"))},
			want: []edgeView{
				{0, "legacy", cols("customer_id"), "users", cols("id"), []EdgeSource{{SourceManual, "manual#1"}}},
				{1, "orders", cols("user_id"), "users", cols("id"), []EdgeSource{{SourceManual, "x"}}},
			},
		},
		{
			name: "no relations at all gives no edges",
			want: []edgeView{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := Build(baseSchema(tt.fks...), tt.manual)
			got := view(g.Edges())
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("edges mismatch\n got: %+v\nwant: %+v", got, tt.want)
			}
			for _, e := range g.Edges() {
				if g.Edge(e.ID) != e {
					t.Errorf("Edge(%d) does not return the edge with that ID", e.ID)
				}
			}
		})
	}
}

func TestBuildDeterministic(t *testing.T) {
	fks := []schema.ForeignKey{
		fk("fk_orders_user", "orders", cols("user_id"), "users", cols("id")),
		fk("fk_legacy_pairs", "legacy", cols("a", "b"), "pairs", cols("a", "b")),
		fk("fk_users_self", "users", cols("email"), "users", cols("email")),
		fk("fk_legacy_user", "legacy", cols("customer_id"), "users", cols("id")),
	}
	man := []relations.ManualRelation{
		manual("m1", "legacy", cols("customer_id"), "users", cols("id")),
		manual("m2", "orders", cols("user_id"), "users", cols("email")),
	}
	want := view(Build(baseSchema(fks...), man).Edges())

	perms := [][]int{{3, 2, 1, 0}, {1, 3, 0, 2}, {2, 0, 3, 1}}
	for _, p := range perms {
		shuffled := make([]schema.ForeignKey, len(fks))
		for i, j := range p {
			shuffled[i] = fks[j]
		}
		revMan := []relations.ManualRelation{man[1], man[0]}
		got := view(Build(baseSchema(shuffled...), revMan).Edges())
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("perm %v: edges differ\n got: %+v\nwant: %+v", p, got, want)
		}
	}
}

func TestBuildDoesNotAliasInput(t *testing.T) {
	childCols := cols("user_id")
	parentCols := cols("id")
	s := baseSchema(fk("fk_orders_user", "orders", childCols, "users", parentCols))
	m := []relations.ManualRelation{manual("", "legacy", cols("customer_id"), "users", cols("id"))}
	g := Build(s, m)
	if m[0].Name != "" {
		t.Errorf("Build mutated manual input: Name = %q", m[0].Name)
	}
	ch := g.ChildrenOf("users")
	if len(ch) != 2 {
		t.Fatalf("ChildrenOf(users) has %d edges, want 2", len(ch))
	}
	e := ch[1] // orders edge
	e.ChildColumns[0] = "changed"
	e.ParentColumns[0] = "changed"
	if childCols[0] != "user_id" || parentCols[0] != "id" {
		t.Errorf("edge columns alias the input slices: %v %v", childCols, parentCols)
	}
}

func TestChildrenOfAndReferencedColumns(t *testing.T) {
	s := baseSchema(
		fk("fk_orders_user", "orders", cols("user_id"), "users", cols("id")),
		fk("fk_users_self", "users", cols("id"), "users", cols("email")), // self-ref, non-PK referenced column
		fk("fk_legacy_pairs", "legacy", cols("a", "b"), "pairs", cols("a", "b")),
	)
	m := []relations.ManualRelation{
		manual("legacy_user", "legacy", cols("customer_id"), "users", cols("id")), // same ParentColumns as fk_orders_user
		manual("legacy_pairs_rev", "legacy", cols("b", "a"), "pairs", cols("b", "a")),
	}
	g := Build(s, m)

	type childView struct {
		Child string
		Cols  []string
	}
	children := func(table string) []childView {
		var out []childView
		prev := -1
		for _, e := range g.ChildrenOf(table) {
			if e.ParentTable != table {
				t.Errorf("ChildrenOf(%q) returned edge with parent %q", table, e.ParentTable)
			}
			if e.ID <= prev {
				t.Errorf("ChildrenOf(%q) not in ID order: %d after %d", table, e.ID, prev)
			}
			prev = e.ID
			out = append(out, childView{e.ChildTable, e.ChildColumns})
		}
		return out
	}

	if got, want := children("users"), []childView{
		{"legacy", cols("customer_id")},
		{"orders", cols("user_id")},
		{"users", cols("id")}, // self-reference is a normal edge
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("ChildrenOf(users) = %+v, want %+v", got, want)
	}
	if got, want := children("pairs"), []childView{
		{"legacy", cols("a", "b")},
		{"legacy", cols("b", "a")},
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("ChildrenOf(pairs) = %+v, want %+v", got, want)
	}
	if got := g.ChildrenOf("orders"); len(got) != 0 {
		t.Errorf("ChildrenOf(orders) = %+v, want empty", got)
	}
	if got := g.ChildrenOf("no_such_table"); len(got) != 0 {
		t.Errorf("ChildrenOf(no_such_table) = %+v, want empty", got)
	}

	refTests := []struct {
		table string
		want  [][]string
	}{
		{"users", [][]string{cols("email"), cols("id")}}, // id deduped across two edges; non-PK email included
		{"pairs", [][]string{cols("a", "b"), cols("b", "a")}},
		{"orders", nil},
		{"no_such_table", nil},
	}
	for _, tt := range refTests {
		got := g.ReferencedColumns(tt.table)
		if len(got) == 0 && len(tt.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ReferencedColumns(%q) = %v, want %v", tt.table, got, tt.want)
		}
	}
}

func TestEdgeHasNoDeleteRule(t *testing.T) {
	// 4.2: ON DELETE rules must never drive behavior, so Edge carries none.
	typ := reflect.TypeOf(Edge{})
	for i := 0; i < typ.NumField(); i++ {
		switch typ.Field(i).Name {
		case "DeleteRule", "OnDelete":
			t.Errorf("Edge has field %s; ON DELETE info must not be on Edge (4.2)", typ.Field(i).Name)
		}
	}
}

func TestBuildFromGoldenFiles(t *testing.T) {
	s, err := schema.Load(filepath.Join("..", "..", "testdata", "schema.golden.8.0.json"))
	if err != nil {
		t.Fatalf("schema.Load: %v", err)
	}
	m, err := relations.Load(filepath.Join("..", "..", "testdata", "relations.sample.yaml"), s)
	if err != nil {
		t.Fatalf("relations.Load: %v", err)
	}
	g := Build(s, m)
	edges := g.Edges()
	if len(edges) != 19 {
		t.Fatalf("got %d edges, want 19 (18 FKs + 1 manual)", len(edges))
	}
	for i, e := range edges {
		if e.ID != i {
			t.Errorf("edges[%d].ID = %d", i, e.ID)
		}
	}
	var found bool
	for _, e := range g.ChildrenOf("users") {
		if e.ChildTable == "legacy_orders" {
			found = true
			if want := []EdgeSource{{SourceManual, "orders_legacy_user"}}; !reflect.DeepEqual(e.Sources, want) {
				t.Errorf("legacy_orders sources = %+v, want %+v", e.Sources, want)
			}
		}
	}
	if !found {
		t.Error("manual relation legacy_orders -> users not found among ChildrenOf(users)")
	}
	// Composite FK (shipment_events -> shipments) and a non-PK referenced column (users.email).
	if got, want := g.ReferencedColumns("shipments"), [][]string{cols("order_id", "seq")}; !reflect.DeepEqual(got, want) {
		t.Errorf("ReferencedColumns(shipments) = %v, want %v", got, want)
	}
	if got, want := g.ReferencedColumns("users"), [][]string{cols("email"), cols("id")}; !reflect.DeepEqual(got, want) {
		t.Errorf("ReferencedColumns(users) = %v, want %v", got, want)
	}
}
