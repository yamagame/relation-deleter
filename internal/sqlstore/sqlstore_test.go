package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/yamagame/relation-deleter/internal/collect"
	"github.com/yamagame/relation-deleter/internal/schema"
)

// recordingQuerier records every query and fails it, so tests can prove that
// a query was (or was not) issued without a real database.
type recordingQuerier struct {
	calls []query
	err   error
}

func (q *recordingQuerier) QueryContext(_ context.Context, s string, args ...any) (*sql.Rows, error) {
	q.calls = append(q.calls, query{SQL: s, Args: args})
	return nil, q.err
}

func testSchema() *schema.Schema {
	return &schema.Schema{
		FormatVersion: 1,
		Tables: []schema.Table{
			{Name: "orders", Columns: []schema.Column{
				{Name: "id", Type: "bigint"}, {Name: "user_id", Type: "bigint"},
			}, PrimaryKey: []string{"id"}},
			{Name: "shipment_events", Columns: []schema.Column{
				{Name: "id", Type: "bigint"}, {Name: "order_id", Type: "bigint"}, {Name: "shipment_seq", Type: "int"},
			}, PrimaryKey: []string{"id"}},
			{Name: "audit_logs", Columns: []schema.Column{
				{Name: "user_id", Type: "bigint"}, {Name: "action", Type: "varchar(32)"},
			}},
			{Name: "we`ird", Columns: []schema.Column{{Name: "c`ol", Type: "int"}}, PrimaryKey: []string{"c`ol"}},
		},
	}
}

func tuples(vals ...any) []collect.Tuple {
	out := make([]collect.Tuple, len(vals))
	for i, v := range vals {
		out[i] = collect.Tuple{v}
	}
	return out
}

func TestBuildQueries(t *testing.T) {
	tests := []struct {
		name    string
		table   string
		columns []string
		keyed   bool
		p       collect.Predicate
		chunk   int
		want    []query
	}{
		{
			name: "keyed single column", table: "orders", columns: []string{"id", "user_id"}, keyed: true,
			p:     collect.Predicate{Columns: []string{"user_id"}, Values: tuples("1", "2")},
			chunk: 500,
			want: []query{{
				SQL:  "SELECT `id`,`user_id` FROM `orders` WHERE `user_id` IN (?,?)",
				Args: []any{"1", "2"},
			}},
		},
		{
			name: "keyed multiple columns", table: "shipment_events", columns: []string{"id"}, keyed: true,
			p: collect.Predicate{Columns: []string{"order_id", "shipment_seq"}, Values: []collect.Tuple{
				{"101", "1"}, {"101", "2"},
			}},
			chunk: 500,
			want: []query{{
				SQL:  "SELECT `id` FROM `shipment_events` WHERE (`order_id`,`shipment_seq`) IN ((?,?),(?,?))",
				Args: []any{"101", "1", "101", "2"},
			}},
		},
		{
			name: "unkeyed groups by all columns", table: "audit_logs", columns: []string{"user_id", "action"}, keyed: false,
			p:     collect.Predicate{Columns: []string{"user_id"}, Values: tuples("1")},
			chunk: 500,
			want: []query{{
				SQL:  "SELECT `user_id`,`action`,COUNT(*) FROM `audit_logs` WHERE `user_id` IN (?) GROUP BY `user_id`,`action`",
				Args: []any{"1"},
			}},
		},
		{
			name: "chunks predicate tuples", table: "orders", columns: []string{"id"}, keyed: true,
			p:     collect.Predicate{Columns: []string{"user_id"}, Values: tuples("1", "2", "3")},
			chunk: 2,
			want: []query{
				{SQL: "SELECT `id` FROM `orders` WHERE `user_id` IN (?,?)", Args: []any{"1", "2"}},
				{SQL: "SELECT `id` FROM `orders` WHERE `user_id` IN (?)", Args: []any{"3"}},
			},
		},
		{
			name: "binary args are passed as-is", table: "orders", columns: []string{"id"}, keyed: true,
			p:     collect.Predicate{Columns: []string{"id"}, Values: tuples([]byte{0x00, 0xff})},
			chunk: 0,
			want:  []query{{SQL: "SELECT `id` FROM `orders` WHERE `id` IN (?)", Args: []any{[]byte{0x00, 0xff}}}},
		},
		{
			name: "identifiers are quoted", table: "we`ird", columns: []string{"c`ol"}, keyed: true,
			p:     collect.Predicate{Columns: []string{"c`ol"}, Values: tuples("1")},
			chunk: 500,
			want:  []query{{SQL: "SELECT `c``ol` FROM `we``ird` WHERE `c``ol` IN (?)", Args: []any{"1"}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildQueries(tt.table, tt.columns, tt.keyed, tt.p, tt.chunk)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildQueries() =\n%#v\nwant\n%#v", got, tt.want)
			}
		})
	}
}

func TestBuildQueriesPlaceholderLimit(t *testing.T) {
	// 3 predicate columns: at most 65535/3 = 21845 tuples per statement.
	vals := make([]collect.Tuple, 21846)
	for i := range vals {
		vals[i] = collect.Tuple{"1", "2", "3"}
	}
	p := collect.Predicate{Columns: []string{"id", "order_id", "shipment_seq"}, Values: vals}
	got := buildQueries("shipment_events", []string{"id"}, true, p, 100000)
	if len(got) != 2 {
		t.Fatalf("got %d statements, want 2", len(got))
	}
	if n := len(got[0].Args); n != 65535 {
		t.Errorf("first statement has %d placeholders, want 65535", n)
	}
	if n := len(got[1].Args); n != 3 {
		t.Errorf("second statement has %d placeholders, want 3", n)
	}
}

func TestEffectiveChunkSize(t *testing.T) {
	tests := []struct{ chunk, ncols, want int }{
		{0, 1, 500},
		{-1, 2, 500},
		{1, 1, 1},
		{2, 3, 2},
		{100000, 1, 65535},
		{100000, 2, 32767},
		{500, 70000, 1},
		{500, 0, 500},
	}
	for _, tt := range tests {
		if got := effectiveChunkSize(tt.chunk, tt.ncols); got != tt.want {
			t.Errorf("effectiveChunkSize(%d, %d) = %d, want %d", tt.chunk, tt.ncols, got, tt.want)
		}
	}
}

func TestFetchRejectsInvalidInputWithoutQuery(t *testing.T) {
	one := tuples("1")
	tests := []struct {
		name    string
		table   string
		columns []string
		keyed   bool
		p       collect.Predicate
		wantErr error
	}{
		{"unknown table", "nope", []string{"id"}, true, collect.Predicate{Columns: []string{"id"}, Values: one}, ErrUnknownTable},
		{"unknown select column", "orders", []string{"id", "nope"}, true, collect.Predicate{Columns: []string{"id"}, Values: one}, ErrUnknownColumn},
		{"unknown predicate column", "orders", []string{"id"}, true, collect.Predicate{Columns: []string{"nope"}, Values: one}, ErrUnknownColumn},
		{"unknown unkeyed column", "audit_logs", []string{"user_id", "nope"}, false, collect.Predicate{Columns: []string{"user_id"}, Values: one}, ErrUnknownColumn},
		{"injection attempt in table", "orders`; DROP TABLE users; --", []string{"id"}, true, collect.Predicate{Columns: []string{"id"}, Values: one}, ErrUnknownTable},
		{"no select columns", "orders", nil, true, collect.Predicate{Columns: []string{"id"}, Values: one}, ErrBadPredicate},
		{"no predicate columns", "orders", []string{"id"}, true, collect.Predicate{Values: one}, ErrBadPredicate},
		{"tuple length mismatch", "orders", []string{"id"}, true, collect.Predicate{Columns: []string{"id"}, Values: []collect.Tuple{{"1", "2"}}}, ErrBadPredicate},
		{"NULL in predicate", "orders", []string{"id"}, true, collect.Predicate{Columns: []string{"id"}, Values: []collect.Tuple{{nil}}}, ErrBadPredicate},
		{"unknown table with empty values", "nope", []string{"id"}, true, collect.Predicate{Columns: []string{"id"}}, ErrUnknownTable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := &recordingQuerier{}
			_, err := NewRowSource(q, testSchema(), 500).Fetch(context.Background(), tt.table, tt.columns, tt.keyed, tt.p)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if len(q.calls) != 0 {
				t.Errorf("issued %d queries, want none: %v", len(q.calls), q.calls)
			}
		})
	}
}

func TestFetchEmptyPredicateIssuesNoQuery(t *testing.T) {
	q := &recordingQuerier{}
	rows, err := NewRowSource(q, testSchema(), 500).Fetch(context.Background(), "orders", []string{"id"}, true,
		collect.Predicate{Columns: []string{"user_id"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %v, want none", rows)
	}
	if len(q.calls) != 0 {
		t.Errorf("issued %d queries, want none", len(q.calls))
	}
}

func TestFetchWrapsQueryErrorWithTable(t *testing.T) {
	boom := errors.New("boom")
	q := &recordingQuerier{err: boom}
	_, err := NewRowSource(q, testSchema(), 500).Fetch(context.Background(), "orders", []string{"id"}, true,
		collect.Predicate{Columns: []string{"user_id"}, Values: tuples("1")})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapping %v", err, boom)
	}
	if !strings.Contains(err.Error(), "orders") {
		t.Errorf("err = %q, want the table name", err)
	}
	if len(q.calls) != 1 {
		t.Fatalf("issued %d queries, want 1", len(q.calls))
	}
	want := query{SQL: "SELECT `id` FROM `orders` WHERE `user_id` IN (?)", Args: []any{"1"}}
	if !reflect.DeepEqual(q.calls[0], want) {
		t.Errorf("query = %#v, want %#v", q.calls[0], want)
	}
}

func TestConvert(t *testing.T) {
	str := schema.Column{Name: "s", Type: "varchar(10)"}
	bin := schema.Column{Name: "b", Type: "binary(16)"}
	raw := sql.RawBytes("ab")
	tests := []struct {
		name string
		col  schema.Column
		raw  sql.RawBytes
		want collect.Value
	}{
		{"string", str, raw, "ab"},
		{"empty string", str, sql.RawBytes{}, ""},
		{"binary", bin, sql.RawBytes{0x00, 0xff}, []byte{0x00, 0xff}},
		{"empty binary is not NULL", bin, sql.RawBytes{}, []byte{}},
		{"NULL string", str, nil, nil},
		{"NULL binary", bin, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convert(tt.col, tt.raw)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("convert() = %#v, want %#v", got, tt.want)
			}
		})
	}
	// The copy must not alias the driver's buffer.
	buf := sql.RawBytes{1, 2}
	got := convert(bin, buf).([]byte)
	buf[0] = 9
	if got[0] != 1 {
		t.Error("binary value aliases the scan buffer")
	}
}
