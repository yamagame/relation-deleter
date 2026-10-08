package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/yamagame/mysql-relation-deleter/internal/collect"
	"github.com/yamagame/mysql-relation-deleter/internal/dbconn"
	"github.com/yamagame/mysql-relation-deleter/internal/plan"
	"github.com/yamagame/mysql-relation-deleter/internal/report"
)

// errExecuteNotImplemented marks the boundary between task 5.2 (the dry-run
// flow) and task 5.3 (the execute flow), which replaces runExecute.
var errExecuteNotImplemented = errors.New("--execute is not available in this build")

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

// runExecute is the execute flow (task 5.3).
func runExecute(ctx context.Context, inv *invocation, io IO, deps Deps, db TxBeginner) int {
	printErrors(io.Err, errExecuteNotImplemented.Error())
	return exitRuntime
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
