package graph

import (
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/yamagame/relation-deleter/internal/relations"
	"github.com/yamagame/relation-deleter/internal/schema"
)

// rel builds a single-column manual relation child.parent_id -> parent.id.
func rel(child, parent string) relations.ManualRelation {
	return manual("", child, cols(parent+"_id"), parent, cols("id"))
}

func graphOf(rels ...relations.ManualRelation) *Graph {
	return Build(&schema.Schema{FormatVersion: 1, Database: "app"}, rels)
}

func grp(cyclic bool, tables ...string) DeleteGroup {
	return DeleteGroup{Tables: tables, Cyclic: cyclic}
}

func TestDeleteOrder(t *testing.T) {
	tests := []struct {
		name   string
		rels   []relations.ManualRelation
		tables []string
		want   []DeleteGroup
	}{
		{
			name:   "multi-level chain is child to parent (6.5)",
			rels:   []relations.ManualRelation{rel("b", "a"), rel("c", "b"), rel("d", "c")},
			tables: []string{"a", "b", "c", "d"},
			want:   []DeleteGroup{grp(false, "d"), grp(false, "c"), grp(false, "b"), grp(false, "a")},
		},
		{
			name: "diamond puts the shared child first and the root last",
			// d -> b -> a, d -> c -> a
			rels:   []relations.ManualRelation{rel("b", "a"), rel("c", "a"), rel("d", "b"), rel("d", "c")},
			tables: []string{"a", "b", "c", "d"},
			want:   []DeleteGroup{grp(false, "d"), grp(false, "b"), grp(false, "c"), grp(false, "a")},
		},
		{
			name:   "self-referencing table is cyclic (6.6)",
			rels:   []relations.ManualRelation{rel("folders", "folders"), rel("files", "folders")},
			tables: []string{"files", "folders"},
			want:   []DeleteGroup{grp(false, "files"), grp(true, "folders")},
		},
		{
			name:   "mutual reference forms one cyclic group (6.6)",
			rels:   []relations.ManualRelation{rel("x", "y"), rel("y", "x")},
			tables: []string{"y", "x"},
			want:   []DeleteGroup{grp(true, "x", "y")},
		},
		{
			name: "cycle with a child and a parent of the cycle",
			// leaf -> m1 <-> m2 -> root
			rels:   []relations.ManualRelation{rel("m1", "m2"), rel("m2", "m1"), rel("leaf", "m1"), rel("m2", "root")},
			tables: []string{"root", "m2", "m1", "leaf"},
			want:   []DeleteGroup{grp(false, "leaf"), grp(true, "m1", "m2"), grp(false, "root")},
		},
		{
			name:   "three-table cycle is one cyclic group",
			rels:   []relations.ManualRelation{rel("p", "q"), rel("q", "r"), rel("r", "p")},
			tables: []string{"r", "q", "p"},
			want:   []DeleteGroup{grp(true, "p", "q", "r")},
		},
		{
			name:   "unconnected tables are ordered by name",
			rels:   []relations.ManualRelation{rel("b", "a")},
			tables: []string{"zeta", "b", "alpha", "a", "mid"},
			want:   []DeleteGroup{grp(false, "alpha"), grp(false, "b"), grp(false, "a"), grp(false, "mid"), grp(false, "zeta")},
		},
		{
			name: "subset ignores edges to tables outside the set",
			// c -> b -> a, and b <-> outside would make a cycle if outside were included
			rels:   []relations.ManualRelation{rel("b", "a"), rel("c", "b"), rel("b", "outside"), rel("outside", "b")},
			tables: []string{"b", "c"},
			want:   []DeleteGroup{grp(false, "c"), grp(false, "b")},
		},
		{
			name:   "duplicate and unknown table names",
			rels:   []relations.ManualRelation{rel("b", "a")},
			tables: []string{"a", "b", "a", "ghost", "b"},
			want:   []DeleteGroup{grp(false, "b"), grp(false, "a"), grp(false, "ghost")},
		},
		{
			name:   "empty input",
			rels:   []relations.ManualRelation{rel("b", "a")},
			tables: nil,
			want:   []DeleteGroup{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := graphOf(tt.rels...)
			in := slices.Clone(tt.tables)
			got := g.DeleteOrder(in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DeleteOrder(%v)\n got  %+v\n want %+v", tt.tables, got, tt.want)
			}
			if !slices.Equal(in, tt.tables) {
				t.Errorf("input was modified: %v", in)
			}
		})
	}
}

func TestDeleteOrderIgnoresInputOrder(t *testing.T) {
	g := graphOf(
		rel("b", "a"), rel("c", "a"), rel("d", "b"), rel("d", "c"),
		rel("m1", "m2"), rel("m2", "m1"), rel("m1", "d"), rel("leaf", "m2"),
		rel("s", "s"), rel("s", "a"),
	)
	tables := []string{"a", "b", "c", "d", "m1", "m2", "leaf", "s", "lonely"}
	want := g.DeleteOrder(tables)
	if len(want) == 0 {
		t.Fatal("DeleteOrder returned no groups")
	}
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 50; i++ {
		shuffled := slices.Clone(tables)
		r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if got := g.DeleteOrder(shuffled); !reflect.DeepEqual(got, want) {
			t.Fatalf("DeleteOrder(%v) = %+v, want %+v", shuffled, got, want)
		}
	}
}

func TestDeleteOrderGolden(t *testing.T) {
	s, err := schema.Load(filepath.Join("..", "..", "testdata", "schema.golden.8.0.json"))
	if err != nil {
		t.Fatalf("schema.Load: %v", err)
	}
	m, err := relations.Load(filepath.Join("..", "..", "testdata", "relations.sample.yaml"), s)
	if err != nil {
		t.Fatalf("relations.Load: %v", err)
	}
	g := Build(s, m)
	tables := make([]string, 0, len(s.Tables))
	for _, tb := range s.Tables {
		tables = append(tables, tb.Name)
	}
	groups := g.DeleteOrder(tables)

	groupOf := make(map[string]int)
	total := 0
	for i, gr := range groups {
		if !slices.IsSorted(gr.Tables) {
			t.Errorf("group %d tables not sorted: %v", i, gr.Tables)
		}
		for _, tb := range gr.Tables {
			if _, dup := groupOf[tb]; dup {
				t.Errorf("table %s appears in more than one group", tb)
			}
			groupOf[tb] = i
			total++
		}
	}
	if total != len(tables) {
		t.Fatalf("groups cover %d tables, want %d", total, len(tables))
	}

	find := func(tables ...string) *DeleteGroup {
		for i := range groups {
			if slices.Equal(groups[i].Tables, tables) {
				return &groups[i]
			}
		}
		return nil
	}
	if gr := find("folders"); gr == nil || !gr.Cyclic {
		t.Errorf("folders group = %+v, want a cyclic singleton", gr)
	}
	if gr := find("team_members", "teams"); gr == nil || !gr.Cyclic {
		t.Errorf("teams/team_members group = %+v, want one cyclic group", gr)
	}
	for _, gr := range groups {
		if len(gr.Tables) == 1 && gr.Cyclic && gr.Tables[0] != "folders" {
			t.Errorf("unexpected cyclic singleton %v", gr.Tables)
		}
	}

	for _, e := range g.Edges() {
		ci, cok := groupOf[e.ChildTable]
		pi, pok := groupOf[e.ParentTable]
		if !cok || !pok || ci == pi {
			continue
		}
		if ci >= pi {
			t.Errorf("edge %s -> %s: child group %d not before parent group %d", e.ChildTable, e.ParentTable, ci, pi)
		}
	}
}
