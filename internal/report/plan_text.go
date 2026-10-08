// Package report renders the deletion plan, the confirmation summary, the
// deletion result, missing-root warnings and progress as text (3.5, 5.2–5.5,
// 6.1, 6.8, 8.2). The plan and results go to stdout; progress and warnings go
// to stderr — the caller chooses the writer. Every renderer is deterministic.
// It depends on plan, collect, graph and execute.
package report

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/yamagame/mysql-relation-deleter/internal/collect"
	"github.com/yamagame/mysql-relation-deleter/internal/execute"
	"github.com/yamagame/mysql-relation-deleter/internal/graph"
	"github.com/yamagame/mysql-relation-deleter/internal/plan"
)

// ErrInconsistent is returned when the plan, collection and graph given to
// RenderPlan do not belong together.
var ErrInconsistent = errors.New("plan, collection and graph are inconsistent")

const (
	fkChecksOff = "SET SESSION foreign_key_checks = 0;"
	fkChecksOn  = "SET SESSION foreign_key_checks = 1;"
)

// RenderPlan writes the whole deletion plan to w without truncation (5.2):
//
//	Deletion plan (child -> parent):
//	N. <table>: <count> rows[ (no primary key)][ [cyclic group G]]
//	   root                                         (the root table)
//	   via FK <name>[, manual <name>...]: child(cols) -> parent(cols)
//	   key <pk literal>                             (PK tables, one per row, RowKey order)
//	   match <col> IN (<literal>,...)               (PK-less tables, one per via edge)
//	   match (<c1>,<c2>) IN ((<l1>,<l2>),...)
//	<blank>
//	Statements (execution order):
//	<SQL with literal values>;                      (wrapped in SET SESSION
//	                                                 foreign_key_checks = 0/1 for cyclic groups)
//	<blank>
//	total=<rows> tables=<tables>
//
// Tables are numbered in deletion order; G is the 1-based index of the plan
// group, so tables deleted together with FK checks off share the same G (6.6).
// "via" lines list the relations that made the table a target in edge ID
// order, each with every source (FK constraint or manual relation) (5.5).
// Literals follow literal: strings are quoted and escaped, []byte is hex. The
// expanded statements are for display only (5.3); totals are 5.4.
func RenderPlan(w io.Writer, p *plan.Plan, c *collect.Collection, g *graph.Graph) error {
	bw := bufio.NewWriter(w)
	fmt.Fprintln(bw, "Deletion plan (child -> parent):")
	num := 0
	for gi, grp := range p.Groups {
		for _, st := range grp.Steps {
			num++
			if err := renderTable(bw, num, gi+1, grp.Cyclic, st, c, g); err != nil {
				return err
			}
		}
	}

	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "Statements (execution order):")
	for _, grp := range p.Groups {
		if len(grp.Steps) == 0 {
			continue // execute.Run skips empty groups, including their SET statements
		}
		if grp.Cyclic {
			fmt.Fprintln(bw, fkChecksOff)
		}
		for _, st := range grp.Steps {
			for _, s := range st.Statements {
				sql, err := expandSQL(s.SQL, s.Args)
				if err != nil {
					return fmt.Errorf("table %s: %w", st.Table, err)
				}
				fmt.Fprintf(bw, "%s;\n", sql)
			}
		}
		if grp.Cyclic {
			fmt.Fprintln(bw, fkChecksOn)
		}
	}

	fmt.Fprintln(bw)
	fmt.Fprintf(bw, "total=%d tables=%d\n", p.Total, p.TableCount)
	return bw.Flush()
}

func renderTable(w io.Writer, num, group int, cyclic bool, st plan.Step, c *collect.Collection, g *graph.Graph) error {
	ts := c.Tables[st.Table]
	if ts == nil {
		return fmt.Errorf("%w: table %s is not in the collection", ErrInconsistent, st.Table)
	}
	fmt.Fprintf(w, "%d. %s: %s", num, st.Table, rows(st.Expected))
	if !ts.Keyed {
		fmt.Fprint(w, " (no primary key)")
	}
	if cyclic {
		fmt.Fprintf(w, " [cyclic group %d]", group)
	}
	fmt.Fprintln(w)

	if ts.Root {
		fmt.Fprintln(w, "   root")
	}
	ids := make([]int, 0, len(ts.Via))
	for id := range ts.Via {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	edges := make([]*graph.Edge, len(ids))
	for i, id := range ids {
		e := g.Edge(id)
		if e == nil || e.ChildTable != ts.Table {
			return fmt.Errorf("%w: table %s has unknown relation %d", ErrInconsistent, ts.Table, id)
		}
		edges[i] = e
		fmt.Fprintf(w, "   via %s: %s(%s) -> %s(%s)\n", sources(e.Sources),
			e.ChildTable, strings.Join(e.ChildColumns, ","), e.ParentTable, strings.Join(e.ParentColumns, ","))
	}

	if ts.Keyed {
		keys := make([]collect.RowKey, 0, len(ts.Rows))
		for k := range ts.Rows {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			s, err := tupleLiteral(ts.Rows[k].Key)
			if err != nil {
				return fmt.Errorf("table %s: %w", ts.Table, err)
			}
			fmt.Fprintf(w, "   key %s\n", s)
		}
		return nil
	}

	for i, e := range edges {
		vals := make([]string, len(ts.Via[ids[i]].Values))
		for j, t := range ts.Via[ids[i]].Values {
			s, err := tupleLiteral(t)
			if err != nil {
				return fmt.Errorf("table %s: %w", ts.Table, err)
			}
			vals[j] = s
		}
		cols := strings.Join(e.ChildColumns, ",")
		if len(e.ChildColumns) > 1 {
			cols = "(" + cols + ")"
		}
		fmt.Fprintf(w, "   match %s IN (%s)\n", cols, strings.Join(vals, ","))
	}
	return nil
}

func sources(src []graph.EdgeSource) string {
	parts := make([]string, len(src))
	for i, s := range src {
		kind := "FK"
		if s.Kind == graph.SourceManual {
			kind = "manual"
		}
		parts[i] = kind + " " + s.Name
	}
	return strings.Join(parts, ", ")
}

func rows(n int64) string {
	if n == 1 {
		return "1 row"
	}
	return fmt.Sprintf("%d rows", n)
}

// RenderSummary writes the per-table expected counts in deletion order and
// the totals, for the confirmation before deleting (6.1):
//
//	Rows to delete:
//	  <table>: <count>
//	total=<rows> tables=<tables>
func RenderSummary(w io.Writer, p *plan.Plan) error {
	bw := bufio.NewWriter(w)
	fmt.Fprintln(bw, "Rows to delete:")
	for _, grp := range p.Groups {
		for _, st := range grp.Steps {
			fmt.Fprintf(bw, "  %s: %d\n", st.Table, st.Expected)
		}
	}
	fmt.Fprintf(bw, "total=%d tables=%d\n", p.Total, p.TableCount)
	return bw.Flush()
}

// RenderResult writes the per-table deleted counts in execution order and
// their totals (6.8):
//
//	Deleted rows:
//	  <table>: <deleted>
//	total=<rows> tables=<tables>
func RenderResult(w io.Writer, rs []execute.TableResult) error {
	bw := bufio.NewWriter(w)
	fmt.Fprintln(bw, "Deleted rows:")
	var total int64
	for _, r := range rs {
		fmt.Fprintf(bw, "  %s: %d\n", r.Table, r.Deleted)
		total += r.Deleted
	}
	fmt.Fprintf(bw, "total=%d tables=%d\n", total, len(rs))
	return bw.Flush()
}

// RenderMissing writes one warning line per root primary key value that was
// not found (3.5), in the given order, e.g.
//
//	warning: root record not found: '42'
//	warning: root record not found: ('7','x')
//
// It writes nothing for an empty list. It is meant for stderr.
func RenderMissing(w io.Writer, missing []collect.Tuple) error {
	bw := bufio.NewWriter(w)
	for _, t := range missing {
		s, err := tupleLiteral(t)
		if err != nil {
			return err
		}
		fmt.Fprintf(bw, "warning: root record not found: %s\n", s)
	}
	return bw.Flush()
}
