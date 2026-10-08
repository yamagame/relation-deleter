// Package sqlstore is the SQL implementation of collect.RowSource. It only
// issues SELECT statements: Querier exposes nothing but QueryContext, so the
// collection phase cannot change the database (4.6). The SQL uses only syntax
// common to MySQL 5.7 and 8.0 (1.7), and every predicate is split into chunks
// so that no statement exceeds the placeholder limit (8.1).
//
// It depends on schema and collect only.
package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/yamagame/mysql-relation-deleter/internal/collect"
	"github.com/yamagame/mysql-relation-deleter/internal/schema"
)

// defaultChunkSize and maxPlaceholders mirror plan.DefaultChunkSize and
// plan.MaxPlaceholders; sqlstore must not import plan.
const (
	defaultChunkSize = 500
	maxPlaceholders  = 65535
)

var (
	// ErrUnknownTable is returned for a table that is not in the schema file.
	ErrUnknownTable = errors.New("unknown table")
	// ErrUnknownColumn is returned for a column (selected or in the
	// predicate) that is not in the table in the schema file.
	ErrUnknownColumn = errors.New("unknown column")
	// ErrBadPredicate is returned for an empty column list, a tuple whose
	// length differs from the predicate columns, or a NULL predicate value.
	ErrBadPredicate = errors.New("invalid predicate")
)

// Querier is the read-only view of a database connection or transaction that
// sqlstore needs. *sql.DB and *sql.Tx satisfy it. It deliberately has no
// ExecContext: sqlstore never writes (4.6).
type Querier interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

type rowSource struct {
	q         Querier
	s         *schema.Schema
	chunkSize int
}

// NewRowSource returns a RowSource that reads from q. Table and column names
// are accepted only when they exist in s, and are quoted with
// schema.QuoteIdent; values are always passed as placeholders. chunkSize is
// the number of predicate tuples per statement (defaultChunkSize when ≤ 0),
// further capped so that chunk × predicate columns ≤ maxPlaceholders.
func NewRowSource(q Querier, s *schema.Schema, chunkSize int) collect.RowSource {
	return &rowSource{q: q, s: s, chunkSize: chunkSize}
}

// Fetch implements collect.RowSource.
//
// keyed=true issues SELECT cols FROM t WHERE pred, and every row has Count 1.
// keyed=false issues SELECT cols, COUNT(*) FROM t WHERE pred GROUP BY cols;
// the caller passes all columns of the table, so identical rows collapse into
// one Row whose Count is the number of duplicates.
//
// Caveat for keyed=false: GROUP BY and IN compare with the column collation.
// Under a case-insensitive collation (such as utf8mb4_unicode_ci) values that
// differ only in case are grouped together and the group's representative
// value is chosen by the server. The representative can differ between chunks
// or between relations, so the same rows may be counted twice under different
// tuples. The count verification after deletion (6.9) detects this and rolls
// back. Likewise, MySQL groups BLOB/TEXT values by their first
// max_sort_length bytes only.
//
// Columns of binary types (schema.Column.IsBinary) are returned as []byte,
// all others as string, and NULL as nil. Results of all chunks are
// concatenated in statement order.
func (r *rowSource) Fetch(ctx context.Context, table string, columns []string, keyed bool, p collect.Predicate) ([]collect.Row, error) {
	cols, err := r.validate(table, columns, p)
	if err != nil {
		return nil, err
	}
	if len(p.Values) == 0 {
		return nil, nil
	}
	var out []collect.Row
	for _, qy := range buildQueries(table, columns, keyed, p, r.chunkSize) {
		rows, err := r.run(ctx, table, cols, keyed, qy)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// validate checks every name against the schema before any query is built,
// and returns the schema columns in the order of columns.
func (r *rowSource) validate(table string, columns []string, p collect.Predicate) ([]schema.Column, error) {
	t, ok := r.s.Table(table)
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownTable, table)
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("%w: %q: no columns to select", ErrBadPredicate, table)
	}
	if len(p.Columns) == 0 {
		return nil, fmt.Errorf("%w: %q: no predicate columns", ErrBadPredicate, table)
	}
	cols := make([]schema.Column, len(columns))
	for i, name := range columns {
		c, ok := t.Column(name)
		if !ok {
			return nil, fmt.Errorf("%w %q in table %q", ErrUnknownColumn, name, table)
		}
		cols[i] = *c
	}
	for _, name := range p.Columns {
		if _, ok := t.Column(name); !ok {
			return nil, fmt.Errorf("%w %q in table %q (predicate)", ErrUnknownColumn, name, table)
		}
	}
	for i, tup := range p.Values {
		if len(tup) != len(p.Columns) {
			return nil, fmt.Errorf("%w: %q: tuple %d has %d values for %d columns",
				ErrBadPredicate, table, i, len(tup), len(p.Columns))
		}
		if slices.Contains(tup, nil) {
			return nil, fmt.Errorf("%w: %q: tuple %d contains NULL", ErrBadPredicate, table, i)
		}
	}
	return cols, nil
}

// run issues one statement and scans its rows.
func (r *rowSource) run(ctx context.Context, table string, cols []schema.Column, keyed bool, qy query) (out []collect.Row, err error) {
	rows, err := r.q.QueryContext(ctx, qy.SQL, qy.Args...)
	if err != nil {
		return nil, fmt.Errorf("select from %q: %w", table, err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("select from %q: close: %w", table, cerr)
		}
	}()

	n := len(cols)
	if !keyed {
		n++ // COUNT(*)
	}
	raw := make([]sql.RawBytes, n)
	dest := make([]any, n)
	for i := range raw {
		dest[i] = &raw[i]
	}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan %q: %w", table, err)
		}
		row := collect.Row{Values: make(collect.Tuple, len(cols)), Count: 1}
		for i, c := range cols {
			row.Values[i] = convert(c, raw[i])
		}
		if !keyed {
			cnt, err := strconv.ParseInt(string(raw[len(cols)]), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("scan %q: COUNT(*): %w", table, err)
			}
			row.Count = cnt
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select from %q: %w", table, err)
	}
	return out, nil
}

// convert copies a scanned value out of the driver's buffer: []byte for a
// binary column, string otherwise, nil for NULL.
func convert(col schema.Column, raw sql.RawBytes) collect.Value {
	if raw == nil {
		return nil
	}
	if col.IsBinary() {
		return append([]byte{}, raw...)
	}
	return string(raw)
}

// query is one SELECT statement with its placeholder arguments.
type query struct {
	SQL  string
	Args []any
}

// buildQueries builds the statements for an already validated request, one
// per chunk of predicate tuples. Identifiers are quoted with
// schema.QuoteIdent; values are passed as-is as placeholder arguments.
func buildQueries(table string, columns []string, keyed bool, p collect.Predicate, chunkSize int) []query {
	quote := func(names []string) string {
		q := make([]string, len(names))
		for i, n := range names {
			q[i] = schema.QuoteIdent(n)
		}
		return strings.Join(q, ",")
	}
	sel := quote(columns)

	var prefix, suffix string
	if keyed {
		prefix = "SELECT " + sel + " FROM " + schema.QuoteIdent(table) + " WHERE "
	} else {
		prefix = "SELECT " + sel + ",COUNT(*) FROM " + schema.QuoteIdent(table) + " WHERE "
		suffix = " GROUP BY " + sel
	}
	n := len(p.Columns)
	var one string
	if n == 1 {
		prefix += quote(p.Columns) + " IN ("
		one = "?"
	} else {
		prefix += "(" + quote(p.Columns) + ") IN ("
		one = "(" + strings.Repeat("?,", n-1) + "?)"
	}

	size := effectiveChunkSize(chunkSize, n)
	out := make([]query, 0, (len(p.Values)+size-1)/size)
	for chunk := range slices.Chunk(p.Values, size) {
		var b strings.Builder
		b.Grow(len(prefix) + len(chunk)*(len(one)+1) + len(suffix) + 1)
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
		b.WriteString(suffix)
		out = append(out, query{SQL: b.String(), Args: args})
	}
	return out
}

// effectiveChunkSize applies the same rule as plan.EffectiveChunkSize:
// max(1, min(chunkSize or defaultChunkSize, maxPlaceholders/ncols)).
func effectiveChunkSize(chunkSize, ncols int) int {
	if chunkSize <= 0 {
		chunkSize = defaultChunkSize
	}
	if ncols < 1 {
		ncols = 1
	}
	return max(1, min(chunkSize, maxPlaceholders/ncols))
}
