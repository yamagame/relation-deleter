// Package graph merges the schema's foreign keys and the manual relation
// definitions into one reference graph (child columns -> parent columns).
// It depends only on schema and relations.
package graph

import (
	"cmp"
	"slices"
	"strconv"

	"github.com/yamagame/mysql-relation-deleter/internal/relations"
	"github.com/yamagame/mysql-relation-deleter/internal/schema"
)

// SourceKind tells where a relation came from.
type SourceKind int

const (
	SourceForeignKey SourceKind = iota
	SourceManual
)

// EdgeSource is one definition of a relation: an FK constraint name or a
// manual relation name.
type EdgeSource struct {
	Kind SourceKind
	Name string
}

// Edge is one reference relation: ChildTable.ChildColumns[i] references
// ParentTable.ParentColumns[i]. When the same relation is defined more than
// once (e.g. an FK and a manual relation), Sources holds every definition
// (2.5). Edge deliberately carries no ON DELETE rule (4.2).
type Edge struct {
	ID            int
	ChildTable    string
	ChildColumns  []string
	ParentTable   string
	ParentColumns []string
	Sources       []EdgeSource // FK sources first, then manual, each by name
}

// Graph is the merged reference graph. Its contents do not depend on the
// order of the input FKs or manual relations.
type Graph struct {
	edges      []*Edge            // sorted by key; edges[i].ID == i
	byParent   map[string][]*Edge // parent table -> edges in ID order
	referenced map[string][][]string
}

// Build merges the FKs of s and the manual relations into a Graph. Both inputs
// must already have been validated (schema.Load, relations.Load). Inputs are
// not modified; column slices are copied.
func Build(s *schema.Schema, manual []relations.ManualRelation) *Graph {
	type key struct{ child, childCols, parent, parentCols string }
	byKey := make(map[key]*Edge)
	add := func(child string, childCols []string, parent string, parentCols []string, src EdgeSource) {
		k := key{child, encode(childCols), parent, encode(parentCols)}
		e, ok := byKey[k]
		if !ok {
			e = &Edge{
				ChildTable:    child,
				ChildColumns:  slices.Clone(childCols),
				ParentTable:   parent,
				ParentColumns: slices.Clone(parentCols),
			}
			byKey[k] = e
		}
		e.Sources = append(e.Sources, src)
	}

	for _, fk := range s.ForeignKeys {
		add(fk.Table, fk.Columns, fk.ReferencedTable, fk.ReferencedColumns, EdgeSource{SourceForeignKey, fk.Name})
	}
	for i, r := range manual {
		name := r.Name
		if name == "" {
			name = "manual#" + strconv.Itoa(i)
		}
		add(r.Child.Table, r.Child.Columns, r.Parent.Table, r.Parent.Columns, EdgeSource{SourceManual, name})
	}

	g := &Graph{
		edges:      make([]*Edge, 0, len(byKey)),
		byParent:   make(map[string][]*Edge),
		referenced: make(map[string][][]string),
	}
	for _, e := range byKey {
		slices.SortFunc(e.Sources, func(a, b EdgeSource) int {
			return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name))
		})
		g.edges = append(g.edges, e)
	}
	slices.SortFunc(g.edges, compareEdges)

	for i, e := range g.edges {
		e.ID = i
		g.byParent[e.ParentTable] = append(g.byParent[e.ParentTable], e)
		refs := g.referenced[e.ParentTable]
		if !slices.ContainsFunc(refs, func(c []string) bool { return slices.Equal(c, e.ParentColumns) }) {
			g.referenced[e.ParentTable] = append(refs, slices.Clone(e.ParentColumns))
		}
	}
	for _, refs := range g.referenced {
		slices.SortFunc(refs, slices.Compare[[]string])
	}
	return g
}

// compareEdges orders edges by (ChildTable, ChildColumns, ParentTable,
// ParentColumns); column lists compare element by element.
func compareEdges(a, b *Edge) int {
	return cmp.Or(
		cmp.Compare(a.ChildTable, b.ChildTable),
		slices.Compare(a.ChildColumns, b.ChildColumns),
		cmp.Compare(a.ParentTable, b.ParentTable),
		slices.Compare(a.ParentColumns, b.ParentColumns),
	)
}

// encode turns a column list into an unambiguous map-key string.
func encode(cols []string) string {
	var b []byte
	for _, c := range cols {
		b = strconv.AppendQuote(b, c)
	}
	return string(b)
}

// Edges returns every edge in ID order. The slice must not be modified.
func (g *Graph) Edges() []*Edge { return g.edges }

// Edge returns the edge with the given ID, or nil if there is none.
func (g *Graph) Edge(id int) *Edge {
	if id < 0 || id >= len(g.edges) {
		return nil
	}
	return g.edges[id]
}

// ChildrenOf returns the edges whose parent is parentTable, in ID order. It
// returns nil for a table that no edge references. Self-references are
// included.
func (g *Graph) ChildrenOf(parentTable string) []*Edge { return g.byParent[parentTable] }

// ReferencedColumns returns the distinct ParentColumns lists of the edges
// whose parent is table, sorted element by element. It returns nil for a
// table that no edge references.
func (g *Graph) ReferencedColumns(table string) [][]string { return g.referenced[table] }
