package report

import (
	"fmt"
	"io"
)

// progressStep is the number of rows between two progress lines of one table.
const progressStep = 1000

// Progress writes throttled progress lines to W, normally stderr (8.2).
// collect and execute report after every fetch or statement; Progress prints a
// line only when the phase or table changes, or when the cumulative count
// reaches a new multiple of 1,000 since the last line for that table. Lines
// look like "collect orders: 1000 rows" and "delete orders: 1000 rows".
//
// A nil Progress or a nil W prints nothing. Write errors are ignored because
// progress is best-effort. Progress is not safe for concurrent use.
type Progress struct {
	W io.Writer

	phase, table string
	last         int64 // count shown in the last line
	started      bool
}

// Collect has the signature of collect.ProgressFunc.
func (p *Progress) Collect(table string, fetched int64) { p.report("collect", table, fetched) }

// Delete has the signature of execute.Executor.Progress.
func (p *Progress) Delete(table string, deleted int64) { p.report("delete", table, deleted) }

func (p *Progress) report(phase, table string, n int64) {
	if p == nil || p.W == nil {
		return
	}
	switched := !p.started || phase != p.phase || table != p.table
	if !switched && n/progressStep <= p.last/progressStep {
		return
	}
	p.started, p.phase, p.table, p.last = true, phase, table, n
	_, _ = fmt.Fprintf(p.W, "%s %s: %d rows\n", phase, table, n)
}
