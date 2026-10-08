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
// The root table must have a primary key. A child table without one is
// fetched with keyed=false: its rows are identified by the tuple of all their
// columns and carry the number of identical rows in Count, and the predicate
// values of each edge are accumulated in Via for the delete (4.4).
func (c *Collector) Collect(ctx context.Context, rootTable string, rootIDs []Tuple) (*Collection, error) {
	t, ok := c.Schema.Table(rootTable)
	if !ok {
		return nil, fmt.Errorf("root table %s not found in schema", rootTable)
	}
	if len(t.PrimaryKey) == 0 {
		return nil, fmt.Errorf("root table %s has no primary key", rootTable)
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
		rows, err := r.fetch(ctx, rootTable, cols, true, Predicate{Columns: t.PrimaryKey, Values: []Tuple{id}})
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
	var predKeys []RowKey // RowKeys of pred.Values
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
		predKeys = append(predKeys, vk)
	}
	if len(pred.Values) == 0 {
		return nil, nil
	}

	child, ok := r.c.Schema.Table(e.ChildTable)
	if !ok {
		return nil, fmt.Errorf("table %s not found in schema", e.ChildTable)
	}
	keyed := len(child.PrimaryKey) > 0
	cols := r.fetchColumns(child)
	rows, err := r.fetch(ctx, e.ChildTable, cols, keyed, pred)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	// Via is recorded even when every fetched row is already collected (5.5).
	ts := r.tableSet(child)
	via := ts.Via[e.ID]
	if via == nil {
		via = &ViaEdge{EdgeID: e.ID}
		ts.Via[e.ID] = via
	}
	if !keyed {
		if via.seen == nil {
			via.seen = make(map[RowKey]bool)
		}
		for i, vk := range predKeys {
			if !via.seen[vk] {
				via.seen[vk] = true
				via.Values = append(via.Values, pred.Values[i])
			}
		}
	}
	return r.add(ts, child, cols, rows)
}

// fetch checks ctx, calls the source, wraps its error with the table name and
// reports progress. Progress counts rows: one per row of a PK table, Count
// per grouped row of a table without a PK.
func (r *run) fetch(ctx context.Context, table string, cols []string, keyed bool, p Predicate) ([]Row, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := r.c.Source.Fetch(ctx, table, cols, keyed, p)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", table, err)
	}
	for _, row := range rows {
		if !keyed && row.Count < 1 {
			return nil, fmt.Errorf("fetch %s: grouped row has count %d", table, row.Count)
		}
		if keyed {
			r.fetched[table]++
		} else {
			r.fetched[table] += row.Count
		}
	}
	if r.c.Progress != nil {
		r.c.Progress(table, r.fetched[table])
	}
	return rows, nil
}

// add inserts the rows not yet in ts and returns their keys. A PK table's row
// is keyed by its PK and always counts as 1. A row of a table without a PK is
// keyed by all its column values (cols is every column then) and counts as
// Row.Count.
//
// When a tuple of a table without a PK is already collected, whether it
// arrives again through another edge or in another chunk of the same fetch,
// its Count is left as it is. Identical rows cannot be told apart, and any
// predicate that matches one copy matches every copy, so each fetch that
// returns the tuple reports all of its copies: adding the Counts would count
// the same rows twice.
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
		var key Tuple
		count := int64(1)
		if ts.Keyed {
			key = make(Tuple, len(t.PrimaryKey))
			for i, col := range t.PrimaryKey {
				key[i] = values[col]
			}
		} else {
			key = slices.Clone(row.Values)
			count = row.Count
		}
		rk, err := keyOf(key)
		if err != nil {
			return nil, fmt.Errorf("table %s: %w", t.Name, err)
		}
		if _, ok := ts.Rows[rk]; ok {
			continue
		}
		ts.Rows[rk] = &RowEntry{Key: key, Count: count, Values: values}
		r.coll.Total += count
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
// order (4.5). For a table without a PK it returns all columns in schema
// order, which already include every referenced column.
func (r *run) fetchColumns(t *schema.Table) []string {
	if len(t.PrimaryKey) == 0 {
		cols := make([]string, len(t.Columns))
		for i, c := range t.Columns {
			cols[i] = c.Name
		}
		return cols
	}
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
