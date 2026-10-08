package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/yamagame/mysql-relation-deleter/internal/collect"
	"github.com/yamagame/mysql-relation-deleter/internal/dbconn"
	"github.com/yamagame/mysql-relation-deleter/internal/execute"
	"github.com/yamagame/mysql-relation-deleter/internal/plan"
	"github.com/yamagame/mysql-relation-deleter/internal/report"
)

// runFlow resolves the connection settings, connects and runs the dry-run or
// the execute flow. Every uncommitted transaction is rolled back and the
// connection is closed on every path.
func runFlow(ctx context.Context, inv *invocation, io IO, deps Deps) int {
	conn := inv.conn
	if conn.Database == nil {
		db := inv.schema.Database // --database falls back to the schema file's database
		conn.Database = &db
	}
	cfg, err := dbconn.Resolve(conn, io.Getenv)
	if err != nil {
		printErrors(io.Err, err.Error())
		return exitInput
	}
	for _, w := range cfg.Warnings {
		printErrors(io.Err, "warning: "+w)
	}

	db, err := deps.Open(ctx, cfg)
	if err != nil {
		printErrors(io.Err, err.Error()) // dbconn names the redacted target, never the password (9.2, 9.3)
		return exitRuntime
	}
	// The adapter implements io.Closer (the io parameter shadows the package).
	if c, ok := db.(interface{ Close() error }); ok {
		defer c.Close()
	}

	if inv.execute {
		return runExecute(ctx, inv, io, deps, db)
	}
	return runDryRun(ctx, inv, io, deps, db)
}

// runExecute is the execute flow (6.1-6.9). It refuses to run without --yes
// when stdin is not a terminal, before opening a transaction (6.4). Otherwise
// it collects and plans in one read-write transaction, prints the summary,
// asks for confirmation unless --yes is given, deletes and verifies the
// counts in the same transaction, and commits only when everything
// succeeded. Every other path rolls the transaction back.
func runExecute(ctx context.Context, inv *invocation, io IO, deps Deps, db TxBeginner) int {
	if !inv.yes && !io.IsTerminal() {
		printErrors(io.Err, "refusing to delete without confirmation on a non-interactive input; use --yes")
		return exitAborted
	}

	// The transaction must be rolled back even when ctx is cancelled, so it
	// does not inherit the cancellation (see execute.Executor.Run).
	tx, err := db.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		printErrors(io.Err, fmt.Sprintf("begin transaction: %v", err))
		return exitRuntime
	}
	done := false // committed or rolled back
	rollback := func() {
		if !done {
			done = true
			_ = tx.Rollback() // the connection is closed right after; nothing else to do
		}
	}
	defer rollback()

	_, p, code := collectAndPlan(ctx, inv, io, deps, tx)
	if code != proceed {
		return code
	}
	if err := report.RenderSummary(io.Out, p); err != nil { // 6.1
		printErrors(io.Err, fmt.Sprintf("print summary: %v", err))
		return exitRuntime
	}
	if !inv.yes && !confirm(io.In, io.Err, p.Total, p.TableCount) { // 6.2, 6.3
		rollback()
		printErrors(io.Err, "aborted; nothing deleted")
		return exitAborted
	}

	progress := &report.Progress{W: io.Err}
	results, err := (&execute.Executor{Tx: tx, Progress: progress.Delete}).Run(ctx, p) // 6.5
	if err != nil {
		rollback()
		printErrors(io.Err, executeFailure(err)...) // 6.7, 6.9
		return exitRuntime
	}
	if err := tx.Commit(); err != nil {
		done = true // database/sql ends the transaction even when Commit fails
		printErrors(io.Err, fmt.Sprintf("commit: %v", err))
		return exitRuntime
	}
	done = true
	if err := report.RenderResult(io.Out, results); err != nil { // 6.8
		printErrors(io.Err, fmt.Sprintf("print result: %v", err))
		return exitRuntime
	}
	return exitOK
}

// executeFailure describes an error of execute.Executor.Run after the
// rollback: the failed table and the cause (6.7), or the tables whose
// deleted counts differ from the plan (6.9).
func executeFailure(err error) []string {
	var mismatch *execute.CountMismatchError
	if errors.As(err, &mismatch) {
		msgs := []string{"deleted row count does not match the plan"}
		for _, m := range mismatch.Mismatches {
			msgs = append(msgs, fmt.Sprintf("  %s: expected %d, deleted %d", m.Table, m.Expected, m.Deleted))
		}
		return append(msgs, "all changes rolled back")
	}
	var execErr *execute.ExecError
	if errors.As(err, &execErr) {
		cause := execErr.Err.Error()
		if err != error(execErr) { // joined with a failed restore of the FK checks
			cause = err.Error()
		}
		return []string{fmt.Sprintf("failed at table %s: %s; all changes rolled back", execErr.Table, cause)}
	}
	return []string{fmt.Sprintf("delete failed: %v; all changes rolled back", err)}
}

// runDryRun collects in a read-only transaction, prints the plan and rolls
// back. It never writes to the database (4.6, 5.1).
func runDryRun(ctx context.Context, inv *invocation, io IO, deps Deps, db TxBeginner) int {
	// The transaction must be rolled back even when ctx is cancelled, so it
	// does not inherit the cancellation.
	tx, err := db.BeginTx(context.WithoutCancel(ctx), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		printErrors(io.Err, fmt.Sprintf("begin read-only transaction: %v", err))
		return exitRuntime
	}
	defer tx.Rollback() //nolint:errcheck // a dry-run has nothing to keep

	c, p, code := collectAndPlan(ctx, inv, io, deps, tx)
	if code != proceed {
		return code
	}
	if err := report.RenderPlan(io.Out, p, c, inv.graph); err != nil {
		printErrors(io.Err, fmt.Sprintf("print plan: %v", err))
		return exitRuntime
	}
	return exitOK
}

// collectAndPlan is the pipeline shared by the dry-run and execute flows:
// collect the roots and descendants through tx, warn about missing roots,
// check the record limit and build the plan. It returns proceed with the
// collection and the plan, or the exit code after printing the reason. It
// only reads through tx.
func collectAndPlan(ctx context.Context, inv *invocation, io IO, deps Deps, tx Tx) (*collect.Collection, *plan.Plan, int) {
	progress := &report.Progress{W: io.Err}
	col := &collect.Collector{
		Graph:    inv.graph,
		Schema:   inv.schema,
		Source:   deps.NewRowSource(tx, inv.schema, inv.chunkSize),
		Progress: progress.Collect,
	}
	c, err := col.Collect(ctx, inv.table, inv.ids)
	if err != nil {
		printErrors(io.Err, err.Error())
		return nil, nil, exitRuntime
	}

	if len(c.MissingRoots) > 0 { // 3.5
		var missing strings.Builder
		if err := report.RenderMissing(&missing, c.MissingRoots); err != nil {
			printErrors(io.Err, err.Error())
			return nil, nil, exitRuntime
		}
		printErrors(io.Err, strings.TrimSuffix(missing.String(), "\n"))
	}
	if len(c.Tables) == 0 { // every root is missing (3.6)
		printErrors(io.Err, fmt.Sprintf("no matching records in %s", inv.table))
		return nil, nil, exitInput
	}

	if inv.maxRecords > 0 && c.Total > int64(inv.maxRecords) { // 0 is unlimited (7.2)
		printErrors(io.Err, fmt.Sprintf("%d records exceed --max-records %d; nothing deleted", c.Total, inv.maxRecords))
		return nil, nil, exitAborted
	}

	p, err := plan.Build(c, inv.graph, inv.schema, plan.Options{ChunkSize: inv.chunkSize})
	if err != nil {
		printErrors(io.Err, fmt.Sprintf("build plan: %v", err))
		return nil, nil, exitRuntime
	}
	return c, p, proceed
}
