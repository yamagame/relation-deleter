// Package collect validates the deletion roots and walks the reference graph
// breadth-first to build the set of records to delete. It reads rows only
// through the RowSource interface and never writes (4.6). It depends on
// schema and graph.
package collect

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Value is a column value: string, or []byte for binary columns
// (schema.Column.IsBinary). NULL is nil.
type Value = any

// Tuple is an ordered list of column values.
type Tuple []Value

// RowKey is the normalized comparison key of a Tuple. It is an injective
// encoding: every value is a type tag followed, for non-NULL values, by a
// length prefix and the raw bytes, so nil, "", []byte{} and values containing
// separator-like bytes never collide.
type RowKey string

// Predicate represents (Columns) IN (Values...). Values never contain NULL.
type Predicate struct {
	Columns []string
	Values  []Tuple
}

// Row is one row returned by a RowSource.
type Row struct {
	Values Tuple // in the order of the requested columns
	Count  int64 // number of identical rows for a table without a PK; always 1 for a PK table
}

// RowEntry is one collected record.
type RowEntry struct {
	Key    Tuple // PK values; all column values for a table without a PK
	Count  int64
	Values map[string]Value // every fetched column (PK and referenced columns) by name
}

// ViaEdge records that a table was reached through the edge EdgeID (5.5).
//
// For a table without a PK, Values accumulates every distinct parent-side
// tuple (in Edge.ChildColumns order) sent in a predicate along this edge
// whose fetch returned rows, in first-sent order; plan deletes such a table
// with (ChildColumns) IN (Values) (4.4). For a PK table Values stays nil to
// save memory: those rows are deleted by their PK.
type ViaEdge struct {
	EdgeID int
	Values []Tuple

	seen map[RowKey]bool // RowKeys of Values, for deduplication
}

// TableSet is the collected records of one table.
type TableSet struct {
	Table string
	Keyed bool // the table has a PK
	Rows  map[RowKey]*RowEntry
	Via   map[int]*ViaEdge // EdgeID -> the relation that reached this table (5.5)
	Root  bool
}

// Collection is the result of Collect.
type Collection struct {
	Tables       map[string]*TableSet
	MissingRoots []Tuple // root IDs not found in the database, in input order (3.5)
	Total        int64   // sum of RowEntry.Count over all tables
}

// ProgressFunc is notified after every Fetch with the cumulative number of
// rows fetched so far from table (8.2). Throttling is the caller's job.
type ProgressFunc func(table string, fetched int64)

// ErrValueType is returned for a value that is not string, []byte or nil.
var ErrValueType = errors.New("unsupported value type")

const (
	tagNull   = 'n'
	tagString = 's'
	tagBytes  = 'b'
)

// keyOf encodes t into its RowKey.
func keyOf(t Tuple) (RowKey, error) {
	var b []byte
	for i, v := range t {
		switch x := v.(type) {
		case nil:
			b = append(b, tagNull)
		case string:
			b = append(b, tagString)
			b = binary.AppendUvarint(b, uint64(len(x)))
			b = append(b, x...)
		case []byte:
			b = append(b, tagBytes)
			b = binary.AppendUvarint(b, uint64(len(x)))
			b = append(b, x...)
		default:
			return "", fmt.Errorf("value %d: %w %T", i, ErrValueType, v)
		}
	}
	return RowKey(b), nil
}
