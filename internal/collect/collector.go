package collect

import (
	"context"
	"fmt"
	"slices"

	"github.com/yamagame/mysql-relation-deleter/internal/graph"
	"github.com/yamagame/mysql-relation-deleter/internal/schema"
)

// Collector collects the roots and all their descendants. It reads through
// Source only and never writes (4.6).
type Collector struct {
	Graph    *graph.Graph
	Schema   *schema.Schema
	Source   RowSource
	Progress ProgressFunc
}

// pending is a BFS queue item: rows newly added to a table.
type pending struct {
	table string
	keys  []RowKey
}

// run holds the state of one Collect call.
type run struct {
	c       *Collector
	coll    *Collection
	fetched map[string]int64
}

// Collect fetches the root rows of rootTable by primary key and then follows
// every edge of the graph breadth-first, collecting the referencing rows until
// no new row appears (4.1, 4.3). NULL references are never followed and the
// edges' nullability and ON DELETE rules are ignored (4.2).
//
// Each root ID is fetched on its own, so an ID that matches no row is
// reported in MissingRoots, in input order, using the database's comparison
// semantics (3.5). A root ID containing NULL matches nothing and is reported
// missing without a fetch. When no root is found, Collect returns a
// Collection with no tables; deciding what that means is the caller's job.
//
// Tables are reached only when they have a primary key; reaching a child
// table without one returns ErrUnkeyedTable wrapped with the table name
// (collecting those tables is task 3.4's scope).
func (c *Collector) Collect(ctx context.Context, rootTable string, rootIDs []Tuple) (*Collection, error) {
	t, ok := c.Schema.Table(rootTable)
	if !ok {
		return nil, fmt.Errorf("root table %s not found in schema", rootTable)
	}
	if len(t.PrimaryKey) == 0 {
		return nil, fmt.Errorf("root table %s: %w", rootTable, ErrUnkeyedTable)
	}
	r := &run{
		c:       c,
		coll:    &Collection{Tables: make(map[string]*TableSet)},
		fetched: make(map[string]int64),
	}
	cols := r.fetchColumns(t)

	var newKeys []RowKey
	for _, id := range rootIDs {
		if len(id) != len(t.PrimaryKey) {
			return nil, fmt.Errorf("root table %s: id %v has %d values, primary key has %d columns", rootTable, id, len(id), len(t.PrimaryKey))
		}
		if slices.Contains(id, nil) {
			r.coll.MissingRoots = append(r.coll.MissingRoots, id)
			continue
		}
		rows, err := r.fetch(ctx, rootTable, cols, Predicate{Columns: t.PrimaryKey, Values: []Tuple{id}})
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			r.coll.MissingRoots = append(r.coll.MissingRoots, id)
			continue
		}
		ts := r.tableSet(t)
		ts.Root = true
		added, err := r.add(ts, t, cols, rows)
		if err != nil {
			return nil, err
		}
		newKeys = append(newKeys, added...)
	}
	if len(newKeys) == 0 {
		return r.coll, nil
	}

	queue := []pending{{rootTable, newKeys}}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, e := range c.Graph.ChildrenOf(p.table) {
			next, err := r.follow(ctx, e, p.keys)
			if err != nil {
				return nil, err
			}
			if len(next) > 0 {
				queue = append(queue, pending{e.ChildTable, next})
			}
		}
	}
	return r.coll, nil
}

// follow fetches the children of the given parent rows along e and returns
// the keys of the child rows that were newly added.
func (r *run) follow(ctx context.Context, e *graph.Edge, parentKeys []RowKey) ([]RowKey, error) {
	parent := r.coll.Tables[e.ParentTable]
	pred := Predicate{Columns: e.ChildColumns}
	seen := make(map[RowKey]bool)
	for _, k := range parentKeys {
		entry := parent.Rows[k]
		tup := make(Tuple, len(e.ParentColumns))
		for i, col := range e.ParentColumns {
			v, ok := entry.Values[col]
			if !ok {
				return nil, fmt.Errorf("table %s: column %s was not fetched", e.ParentTable, col)
			}
			tup[i] = v
		}
		if slices.Contains(tup, nil) {
			continue // NULL never matches
		}
		vk, err := keyOf(tup)
		if err != nil {
			return nil, fmt.Errorf("table %s: %w", e.ParentTable, err)
		}
		if seen[vk] {
			continue
		}
		seen[vk] = true
		pred.Values = append(pred.Values, tup)
	}
	if len(pred.Values) == 0 {
		return nil, nil
	}

	child, ok := r.c.Schema.Table(e.ChildTable)
	if !ok {
		return nil, fmt.Errorf("table %s not found in schema", e.ChildTable)
	}
	if len(child.PrimaryKey) == 0 {
		return nil, fmt.Errorf("table %s: %w", e.ChildTable, ErrUnkeyedTable)
	}
	cols := r.fetchColumns(child)
	rows, err := r.fetch(ctx, e.ChildTable, cols, pred)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	ts := r.tableSet(child)
	if ts.Via[e.ID] == nil {
		ts.Via[e.ID] = &ViaEdge{EdgeID: e.ID}
	}
	return r.add(ts, child, cols, rows)
}

// fetch checks ctx, calls the source, wraps its error with the table name and
// reports progress.
func (r *run) fetch(ctx context.Context, table string, cols []string, p Predicate) ([]Row, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := r.c.Source.Fetch(ctx, table, cols, true, p)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", table, err)
	}
	r.fetched[table] += int64(len(rows))
	if r.c.Progress != nil {
		r.c.Progress(table, r.fetched[table])
	}
	return rows, nil
}

// add inserts the rows not yet in ts and returns their keys. A PK table's
// row always counts as 1.
func (r *run) add(ts *TableSet, t *schema.Table, cols []string, rows []Row) ([]RowKey, error) {
	var added []RowKey
	for _, row := range rows {
		if len(row.Values) != len(cols) {
			return nil, fmt.Errorf("fetch %s: row has %d values, requested %d columns", t.Name, len(row.Values), len(cols))
		}
		values := make(map[string]Value, len(cols))
		for i, col := range cols {
			values[col] = row.Values[i]
		}
		key := make(Tuple, len(t.PrimaryKey))
		for i, col := range t.PrimaryKey {
			key[i] = values[col]
		}
		rk, err := keyOf(key)
		if err != nil {
			return nil, fmt.Errorf("table %s: %w", t.Name, err)
		}
		if _, ok := ts.Rows[rk]; ok {
			continue
		}
		ts.Rows[rk] = &RowEntry{Key: key, Count: 1, Values: values}
		r.coll.Total++
		added = append(added, rk)
	}
	return added, nil
}

// tableSet returns the TableSet of t, creating it on first use.
func (r *run) tableSet(t *schema.Table) *TableSet {
	ts := r.coll.Tables[t.Name]
	if ts == nil {
		ts = &TableSet{
			Table: t.Name,
			Keyed: len(t.PrimaryKey) > 0,
			Rows:  make(map[RowKey]*RowEntry),
			Via:   make(map[int]*ViaEdge),
		}
		r.coll.Tables[t.Name] = ts
	}
	return ts
}

// fetchColumns returns the PK columns followed by every column the table is
// referenced by (graph.ReferencedColumns), without duplicates and in a stable
// order (4.5).
func (r *run) fetchColumns(t *schema.Table) []string {
	cols := slices.Clone(t.PrimaryKey)
	for _, ref := range r.c.Graph.ReferencedColumns(t.Name) {
		for _, col := range ref {
			if !slices.Contains(cols, col) {
				cols = append(cols, col)
			}
		}
	}
	return cols
}
