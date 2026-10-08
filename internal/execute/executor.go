// Package execute runs a plan.Plan inside a transaction owned by the caller
// and verifies the number of deleted rows per table (6.5, 6.6, 6.7, 6.9). It
// never commits or rolls back: the caller rolls back whenever Run returns an
// error. It depends on plan and the standard library only.
package execute

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/yamagame/relation-deleter/internal/plan"
)

const (
	disableFKChecks = "SET SESSION foreign_key_checks = 0"
	enableFKChecks  = "SET SESSION foreign_key_checks = 1"
)

// Execer runs one statement. *sql.Tx satisfies it.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// TableResult is the outcome of one plan.Step.
type TableResult struct {
	Table    string
	Expected int64
	Deleted  int64 // sum of RowsAffected over the step's statements
}

// ExecError reports the table whose statement failed (6.7).
type ExecError struct {
	Table string
	Err   error
}

func (e *ExecError) Error() string {
	return fmt.Sprintf("table %s: %v", e.Table, e.Err)
}

func (e *ExecError) Unwrap() error { return e.Err }

// CountMismatchError lists the tables whose deleted row count differs from the
// plan (6.9).
type CountMismatchError struct {
	Mismatches []TableResult
}

func (e *CountMismatchError) Error() string {
	parts := make([]string, len(e.Mismatches))
	for i, m := range e.Mismatches {
		parts[i] = fmt.Sprintf("%s: expected %d, deleted %d", m.Table, m.Expected, m.Deleted)
	}
	return "deleted row count does not match the plan: " + strings.Join(parts, "; ")
}

// Executor runs plans on Tx.
type Executor struct {
	Tx Execer
	// Progress, when non-nil, is called after each statement with the table
	// and the number of rows deleted from it so far.
	Progress func(table string, deleted int64)
}

// Run executes the groups of p in order, child -> parent. Around a cyclic group
// it disables the session's FK checks and enables them again afterwards, also
// when a statement of the group fails (6.6). A failing statement stops the run
// with an *ExecError (6.7); when every statement succeeded but some table's
// count differs from the plan, Run returns a *CountMismatchError (6.9). The
// results so far are returned in both cases. Run does not commit or roll back.
//
// Cancelling ctx stops the run before the next statement, and the checks are
// still re-enabled. If the context given to BeginTx is itself cancelled,
// database/sql rolls the transaction back and the restore can fail with
// sql.ErrTxDone. Callers must not reuse the connection after an error (the CLI
// is one-shot and rolls back).
func (e *Executor) Run(ctx context.Context, p *plan.Plan) ([]TableResult, error) {
	results := []TableResult{}
	for _, g := range p.Groups {
		if len(g.Steps) == 0 {
			continue
		}
		var err error
		results, err = e.runGroup(ctx, g, results)
		if err != nil {
			return results, err
		}
	}

	var mismatches []TableResult
	for _, r := range results {
		if r.Deleted != r.Expected {
			mismatches = append(mismatches, r)
		}
	}
	if len(mismatches) > 0 {
		return results, &CountMismatchError{Mismatches: mismatches}
	}
	return results, nil
}

func (e *Executor) runGroup(ctx context.Context, g plan.Group, results []TableResult) ([]TableResult, error) {
	if !g.Cyclic {
		return e.runSteps(ctx, g.Steps, results)
	}

	first, last := g.Steps[0].Table, g.Steps[len(g.Steps)-1].Table
	if err := e.exec(ctx, disableFKChecks); err != nil {
		return results, &ExecError{Table: first, Err: fmt.Errorf("disable foreign_key_checks before group starting at this table: %w", err)}
	}
	results, runErr := e.runSteps(ctx, g.Steps, results)
	// Restore even when ctx is canceled: the session must not keep the checks
	// off, whatever the caller does with the transaction next.
	if _, err := e.Tx.ExecContext(context.WithoutCancel(ctx), enableFKChecks); err != nil {
		restoreErr := fmt.Errorf("re-enable foreign_key_checks after group ending at table %s: %w", last, err)
		if runErr != nil {
			return results, errors.Join(runErr, restoreErr)
		}
		return results, &ExecError{Table: last, Err: restoreErr}
	}
	return results, runErr
}

func (e *Executor) runSteps(ctx context.Context, steps []plan.Step, results []TableResult) ([]TableResult, error) {
	for _, st := range steps {
		results = append(results, TableResult{Table: st.Table, Expected: st.Expected})
		r := &results[len(results)-1]
		for _, s := range st.Statements {
			if err := ctx.Err(); err != nil {
				return results, &ExecError{Table: st.Table, Err: fmt.Errorf("delete canceled: %w", err)}
			}
			res, err := e.Tx.ExecContext(ctx, s.SQL, s.Args...)
			if err != nil {
				return results, &ExecError{Table: st.Table, Err: fmt.Errorf("delete failed: %w", err)}
			}
			n, err := res.RowsAffected()
			if err != nil {
				return results, &ExecError{Table: st.Table, Err: fmt.Errorf("delete failed: rows affected: %w", err)}
			}
			r.Deleted += n
			if e.Progress != nil {
				e.Progress(st.Table, r.Deleted)
			}
		}
	}
	return results, nil
}

func (e *Executor) exec(ctx context.Context, query string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := e.Tx.ExecContext(ctx, query)
	return err
}
