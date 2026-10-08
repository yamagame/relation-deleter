// Package cli is the command-line entry point of relation-deleter: it parses
// the flags, validates every input before touching the database, drives the
// collect, plan and execute steps, and decides the exit code.
package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/yamagame/relation-deleter/internal/collect"
	"github.com/yamagame/relation-deleter/internal/dbconn"
	"github.com/yamagame/relation-deleter/internal/execute"
	"github.com/yamagame/relation-deleter/internal/graph"
	"github.com/yamagame/relation-deleter/internal/relations"
	"github.com/yamagame/relation-deleter/internal/schema"
	"github.com/yamagame/relation-deleter/internal/sqlstore"
)

// Exit codes (design "System Flows > 終了コード").
const (
	exitOK      = 0 // dry-run plan printed, or deletion completed
	exitRuntime = 1 // connection, SQL, delete or count-mismatch error
	exitInput   = 2 // invalid flags, schema, relations, table or ids; no rows found
	exitAborted = 3 // confirmation declined, record limit exceeded, no TTY without --yes
)

// proceed is returned by parseInvocation when the input is valid and Run
// continues with the database flow. It is not an exit code.
const proceed = -1

// IO is the process I/O. IsTerminal reports whether In is an interactive
// terminal; Getenv reads the environment (MYSQL_PWD).
type IO struct {
	In         io.Reader
	Out, Err   io.Writer
	IsTerminal func() bool
	Getenv     func(string) string
}

// StdIO returns the IO of the current process.
func StdIO() IO {
	return IO{
		In:         os.Stdin,
		Out:        os.Stdout,
		Err:        os.Stderr,
		IsTerminal: stdinIsTerminal,
		Getenv:     os.Getenv,
	}
}

// TxBeginner opens transactions. The production implementation wraps
// *sql.DB; tests inject fakes.
type TxBeginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (Tx, error)
}

// Tx is the transaction the flow reads and deletes through. *sql.Tx
// satisfies it.
type Tx interface {
	sqlstore.Querier
	execute.Execer
	Commit() error
	Rollback() error
}

// Deps is the seam for the database side effects. Production uses dbconn
// and sqlstore (DefaultDeps); tests inject fakes.
type Deps struct {
	Open         func(ctx context.Context, c dbconn.Config) (TxBeginner, error)
	NewRowSource func(q sqlstore.Querier, s *schema.Schema, chunkSize int) collect.RowSource
}

// DefaultDeps returns the production dependencies: dbconn.Open wrapped in an
// adapter whose BeginTx returns Tx, and sqlstore.NewRowSource.
func DefaultDeps() Deps {
	return Deps{
		Open: func(ctx context.Context, c dbconn.Config) (TxBeginner, error) {
			db, err := dbconn.Open(ctx, c)
			if err != nil {
				return nil, err
			}
			return &sqlDB{db: db}, nil
		},
		NewRowSource: sqlstore.NewRowSource,
	}
}

// sqlDB adapts *sql.DB to TxBeginner. It also implements io.Closer so the
// flow can release the pool.
type sqlDB struct{ db *sql.DB }

func (d *sqlDB) BeginTx(ctx context.Context, opts *sql.TxOptions) (Tx, error) {
	tx, err := d.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err // never a non-nil interface holding a nil *sql.Tx
	}
	return tx, nil
}

func (d *sqlDB) Close() error { return d.db.Close() }

// invocation is a validated command line with its loaded input files.
type invocation struct {
	table      string
	ids        []collect.Tuple // deduplicated, in first-seen order; values are strings
	execute    bool
	yes        bool
	maxRecords int // 0 means no limit
	chunkSize  int
	conn       dbconn.Flags

	schema    *schema.Schema
	relations []relations.ManualRelation // nil without --relations
	graph     *graph.Graph
}

// Run executes relation-deleter with args (without the program name) and
// returns the exit code. Every input check finishes before deps.Open is
// called (2.3, 2.4, 3.3, 3.4, 3.7).
func Run(ctx context.Context, args []string, io IO, deps Deps) int {
	inv, code := parseInvocation(args, io)
	if code != proceed {
		return code
	}
	return runFlow(ctx, inv, io, deps)
}

// parseInvocation parses and validates args and loads the input files. It
// returns proceed and the invocation when the input is valid; otherwise it
// has printed the help or the errors and returns the exit code.
//
// Errors are collected and printed together, one per line: flag value
// errors, then schema or relations errors, then table and id errors.
func parseInvocation(args []string, io IO) (*invocation, int) {
	var r rawFlags
	fs := newFlagSet(&r)
	if passwordFlagGiven(fs, args) {
		printErrors(io.Err, errPasswordFlag.Error())
		return nil, exitInput
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(io.Out)
			return nil, exitOK
		}
		printErrors(io.Err, err.Error())
		printUsageHint(io.Err)
		return nil, exitInput
	}

	var flagErrs []string
	if fs.NArg() > 0 {
		flagErrs = append(flagErrs, fmt.Sprintf("unexpected argument %q: positional arguments are not accepted", fs.Arg(0)))
	}
	if r.schemaPath == "" {
		flagErrs = append(flagErrs, "--schema is required")
	}
	if r.table == "" {
		flagErrs = append(flagErrs, "--table is required")
	}
	if len(r.ids) == 0 {
		flagErrs = append(flagErrs, "at least one --id is required")
	}
	if r.maxRecords < 0 {
		flagErrs = append(flagErrs, fmt.Sprintf("--max-records must be 0 or greater, got %d", r.maxRecords))
	}
	if r.chunkSize <= 0 {
		flagErrs = append(flagErrs, fmt.Sprintf("--chunk-size must be greater than 0, got %d", r.chunkSize))
	}
	var ids []parsedID
	for _, raw := range r.ids {
		fields, err := parseID(raw)
		if err != nil {
			flagErrs = append(flagErrs, fmt.Sprintf("--id %q: %v", raw, err))
			continue
		}
		t := make(collect.Tuple, len(fields))
		for i, f := range fields {
			t[i] = f
		}
		ids = append(ids, parsedID{raw: raw, values: t})
	}

	inv := &invocation{
		table:      r.table,
		execute:    r.execute,
		yes:        r.yes,
		maxRecords: r.maxRecords,
		chunkSize:  r.chunkSize,
		conn:       connFlags(fs, &r),
	}
	inputErrs := inv.load(r.schemaPath, r.relationsPath, ids)

	if len(flagErrs)+len(inputErrs) > 0 {
		printErrors(io.Err, flagErrs...)
		printErrors(io.Err, inputErrs...)
		if len(flagErrs) > 0 {
			printUsageHint(io.Err)
		}
		return nil, exitInput
	}
	return inv, proceed
}

// load reads the schema and relations files and checks the root table and
// ids against the schema. It returns every problem found; on success it
// fills inv.schema, inv.relations, inv.graph and inv.ids.
func (inv *invocation) load(schemaPath, relationsPath string, ids []parsedID) []string {
	if schemaPath == "" {
		return nil
	}
	s, err := schema.Load(schemaPath)
	if err != nil {
		return []string{err.Error()}
	}

	var errs []string
	var rels []relations.ManualRelation
	if relationsPath != "" {
		rels, err = relations.Load(relationsPath, s)
		var ves relations.ValidationErrors
		switch {
		case errors.As(err, &ves):
			for _, ve := range ves {
				errs = append(errs, ve.Error())
			}
		case err != nil:
			errs = append(errs, err.Error())
		}
	}

	if inv.table != "" {
		var rootErrs []string
		ids, rootErrs = checkRoots(s, schemaPath, inv.table, ids)
		errs = append(errs, rootErrs...)
	}
	if len(errs) > 0 {
		return errs
	}

	inv.schema = s
	inv.relations = rels
	inv.graph = graph.Build(s, rels)
	ids = dedupeIDs(ids) // after conversion, so 0x6162 and "ab" stay distinct
	inv.ids = make([]collect.Tuple, len(ids))
	for i, id := range ids {
		inv.ids[i] = id.values
	}
	return nil
}

// checkRoots verifies that table exists, has a primary key, and that every
// id has one value per primary key column (3.3, 3.4). It returns the ids
// with hex values of binary primary key columns decoded to []byte, and
// every problem found (an invalid hex value names the id and the column).
func checkRoots(s *schema.Schema, schemaPath, table string, ids []parsedID) ([]parsedID, []string) {
	t, ok := s.Table(table)
	if !ok {
		return nil, []string{fmt.Sprintf("table %q not found in schema file %s", table, schemaPath)}
	}
	pk := t.PrimaryKey
	if len(pk) == 0 {
		return nil, []string{fmt.Sprintf("table %q has no primary key; rows of such a table cannot be selected with --id", table)}
	}
	var errs []string
	out := make([]parsedID, 0, len(ids))
	for _, id := range ids {
		if len(id.values) != len(pk) {
			errs = append(errs, fmt.Sprintf("--id %q: table %q has %s (%s), got %s",
				id.raw, table, plural(len(pk), "primary key column"), strings.Join(pk, ", "), plural(len(id.values), "value")))
			continue
		}
		conv := make(collect.Tuple, len(pk))
		bad := false
		for i, name := range pk {
			col, _ := t.Column(name) // schema.Load guarantees PK columns exist
			v, err := rootValue(*col, id.values[i].(string))
			if err != nil {
				errs = append(errs, fmt.Sprintf("--id %q: column %q: %v", id.raw, name, err))
				bad = true
				continue
			}
			conv[i] = v
		}
		if !bad {
			out = append(out, parsedID{raw: id.raw, values: conv})
		}
	}
	return out, errs
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// printErrors writes each message to w with the program prefix, one line
// per message line.
func printErrors(w io.Writer, msgs ...string) {
	for _, m := range msgs {
		for _, line := range strings.Split(m, "\n") {
			fmt.Fprintf(w, "%s: %s\n", progName, line)
		}
	}
}
