// Package plan turns a collect.Collection and the delete order of the
// reference graph into the ordered list of DELETE statements to execute
// (5.3, 6.5). Every value is passed as a placeholder argument, and values are
// split into chunks so a statement never exceeds MySQL's placeholder limit
// (8.1). It depends on collect, graph and schema.
package plan

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/yamagame/mysql-relation-deleter/internal/collect"
	"github.com/yamagame/mysql-relation-deleter/internal/graph"
	"github.com/yamagame/mysql-relation-deleter/internal/schema"
)

// Statement is one DELETE statement with placeholders and its arguments.
type Statement struct {
	SQL  string // with placeholders
	Args []any
}

// Step deletes the collected rows of one table.
type Step struct {
	Table      string
	Expected   int64       // number of rows this table loses
	Statements []Statement // one per chunk
}

// Group is the steps of one DeleteGroup, in DeleteGroup.Tables order.
type Group struct {
	Cyclic bool // FK checks must be disabled while executing the group (6.6)
	Steps  []Step
}

// Plan is the whole deletion, child -> parent.
type Plan struct {
	Groups     []Group // child -> parent
	Total      int64
	TableCount int
}

// Options tunes Build.
type Options struct {
	// ChunkSize is the maximum number of tuples per statement. Zero or a
	// negative value means DefaultChunkSize. It is shrunk automatically so
	// that ChunkSize × columns ≤ MaxPlaceholders.
	ChunkSize int
}

const (
	// DefaultChunkSize is the number of tuples per statement by default.
	DefaultChunkSize = 500
	// MaxPlaceholders is MySQL's limit of placeholders in one prepared statement.
	MaxPlaceholders = 65535
)

var (
	ErrUnknownTable  = errors.New("table not found in schema")
	ErrKeyLength     = errors.New("tuple length does not match the column count")
	ErrNoVia         = errors.New("table without a primary key has no relation to delete by")
	ErrNullValue     = errors.New("NULL value in a delete predicate")
	ErrBadEdge       = errors.New("invalid relation for table")
	ErrTotalMismatch = errors.New("plan total does not match the collection total")
)

// EffectiveChunkSize returns the number of tuples per statement for tuples of
// ncols columns: chunkSize (DefaultChunkSize when ≤ 0) capped so that the
// number of placeholders stays within MaxPlaceholders. It is at least 1.
func EffectiveChunkSize(chunkSize, ncols int) int {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	if ncols < 1 {
		ncols = 1
	}
	return max(1, min(chunkSize, MaxPlaceholders/ncols))
}

// Build creates the deletion plan for c. Groups follow g.DeleteOrder of the
// collected tables and keep their Cyclic flag; steps follow the group's table
// order. A PK table is deleted by its primary key, rows sorted by RowKey. A
// table without a PK is deleted by (ChildColumns) IN (Via values) once per
// Via edge in edge ID order; rows matched by several edges are deleted by the
// first statement that matches them, so the affected rows add up to Expected
// (4.4). Tables without collected rows produce no step. The same input always
// gives the same plan.
func Build(c *collect.Collection, g *graph.Graph, s *schema.Schema, o Options) (*Plan, error) {
	names := make([]string, 0, len(c.Tables))
	for name, ts := range c.Tables {
		if len(ts.Rows) > 0 {
			names = append(names, name)
		}
	}
	slices.Sort(names)

	p := &Plan{Groups: []Group{}}
	for _, dg := range g.DeleteOrder(names) {
		group := Group{Cyclic: dg.Cyclic, Steps: make([]Step, 0, len(dg.Tables))}
		for _, name := range dg.Tables {
			st, err := buildStep(c.Tables[name], g, s, o)
			if err != nil {
				return nil, err
			}
			group.Steps = append(group.Steps, st)
			p.Total += st.Expected
			p.TableCount++
		}
		p.Groups = append(p.Groups, group)
	}
	if p.Total != c.Total {
		return nil, fmt.Errorf("%w: %d != %d", ErrTotalMismatch, p.Total, c.Total)
	}
	return p, nil
}

func buildStep(ts *collect.TableSet, g *graph.Graph, s *schema.Schema, o Options) (Step, error) {
	t, ok := s.Table(ts.Table)
	if !ok {
		return Step{}, fmt.Errorf("%w: %q", ErrUnknownTable, ts.Table)
	}
	st := Step{Table: ts.Table}

	if ts.Keyed {
		if len(t.PrimaryKey) == 0 {
			return Step{}, fmt.Errorf("%w: %q is collected as keyed but has no primary key", ErrKeyLength, ts.Table)
		}
		keys := make([]collect.RowKey, 0, len(ts.Rows))
		for k := range ts.Rows {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		tuples := make([]collect.Tuple, len(keys))
		for i, k := range keys {
			tuples[i] = ts.Rows[k].Key
		}
		stmts, err := statements(ts.Table, t.PrimaryKey, tuples, o)
		if err != nil {
			return Step{}, err
		}
		st.Statements = stmts
		st.Expected = int64(len(ts.Rows))
		return st, nil
	}

	if len(ts.Via) == 0 {
		return Step{}, fmt.Errorf("%w: %q", ErrNoVia, ts.Table)
	}
	ids := make([]int, 0, len(ts.Via))
	for id := range ts.Via {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		e := g.Edge(id)
		if e == nil || e.ChildTable != ts.Table {
			return Step{}, fmt.Errorf("%w: %q edge %d", ErrBadEdge, ts.Table, id)
		}
		stmts, err := statements(ts.Table, e.ChildColumns, ts.Via[id].Values, o)
		if err != nil {
			return Step{}, err
		}
		st.Statements = append(st.Statements, stmts...)
	}
	if len(st.Statements) == 0 {
		return Step{}, fmt.Errorf("%w: %q has no predicate values", ErrNoVia, ts.Table)
	}
	for _, r := range ts.Rows {
		st.Expected += r.Count
	}
	return st, nil
}

// statements renders DELETE FROM table WHERE cols IN (tuples), chunked.
func statements(table string, cols []string, tuples []collect.Tuple, o Options) ([]Statement, error) {
	n := len(cols)
	for _, tup := range tuples {
		if len(tup) != n {
			return nil, fmt.Errorf("%w: %q has %d values for %d columns", ErrKeyLength, table, len(tup), n)
		}
		for i, v := range tup {
			if v == nil {
				return nil, fmt.Errorf("%w: %q column %q", ErrNullValue, table, cols[i])
			}
		}
	}

	quoted := make([]string, n)
	for i, col := range cols {
		quoted[i] = schema.QuoteIdent(col)
	}
	var prefix, one string
	if n == 1 {
		prefix = "DELETE FROM " + schema.QuoteIdent(table) + " WHERE " + quoted[0] + " IN ("
		one = "?"
	} else {
		prefix = "DELETE FROM " + schema.QuoteIdent(table) + " WHERE (" + strings.Join(quoted, ",") + ") IN ("
		one = "(" + strings.Repeat("?,", n-1) + "?)"
	}

	size := EffectiveChunkSize(o.ChunkSize, n)
	out := make([]Statement, 0, (len(tuples)+size-1)/size)
	for chunk := range slices.Chunk(tuples, size) {
		var b strings.Builder
		b.Grow(len(prefix) + len(chunk)*(len(one)+1) + 1)
		b.WriteString(prefix)
		args := make([]any, 0, len(chunk)*n)
		for i, tup := range chunk {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(one)
			args = append(args, tup...)
		}
		b.WriteByte(')')
		out = append(out, Statement{SQL: b.String(), Args: args})
	}
	return out, nil
}
