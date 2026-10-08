package collect

import "context"

// RowSource fetches the rows of table that match p, returning the requested
// columns in order. Chunking the predicate is the implementation's job.
// With keyed=false (a table without a PK) the implementation returns rows
// grouped by all columns, with Count set to the number of identical rows.
//
// A RowSource is read-only by contract: it must never modify the database
// (4.6). Collect uses nothing but Fetch.
type RowSource interface {
	Fetch(ctx context.Context, table string, columns []string, keyed bool, p Predicate) ([]Row, error)
}
