//go:build integration

package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/yamagame/mysql-relation-deleter/internal/collect"
	"github.com/yamagame/mysql-relation-deleter/internal/schema"
	"github.com/yamagame/mysql-relation-deleter/internal/testutil/mysqltest"
)

// countingQuerier forwards to the real database and counts the queries.
type countingQuerier struct {
	q     Querier
	calls int
}

func (c *countingQuerier) QueryContext(ctx context.Context, s string, args ...any) (*sql.Rows, error) {
	c.calls++
	return c.q.QueryContext(ctx, s, args...)
}

// loadGolden loads the reviewed schema file of the target's MySQL version,
// which describes the shared fixture database "app".
func loadGolden(t *testing.T, target mysqltest.Target) *schema.Schema {
	t.Helper()
	s, err := schema.Load(filepath.Join("..", "..", "testdata", "schema.golden."+target.Name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func one(vals ...any) []collect.Tuple {
	out := make([]collect.Tuple, len(vals))
	for i, v := range vals {
		out[i] = collect.Tuple{v}
	}
	return out
}

// sortRows orders rows by their Go representation so results can be compared
// as sets regardless of the server's row order.
func sortRows(rows []collect.Row) []collect.Row {
	out := slices.Clone(rows)
	slices.SortFunc(out, func(a, b collect.Row) int {
		x, y := fmt.Sprintf("%#v", a), fmt.Sprintf("%#v", b)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	})
	return out
}

func r(vals ...any) collect.Row { return collect.Row{Values: collect.Tuple(vals), Count: 1} }

func rc(count int64, vals ...any) collect.Row {
	return collect.Row{Values: collect.Tuple(vals), Count: count}
}

func fetch(t *testing.T, src collect.RowSource, table string, cols []string, keyed bool, p collect.Predicate) []collect.Row {
	t.Helper()
	rows, err := src.Fetch(context.Background(), table, cols, keyed, p)
	if err != nil {
		t.Fatalf("Fetch(%s): %v", table, err)
	}
	return rows
}

func assertRows(t *testing.T, got, want []collect.Row) {
	t.Helper()
	got, want = sortRows(got), sortRows(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows =\n%#v\nwant\n%#v", got, want)
	}
}

func TestIntegrationFetch(t *testing.T) {
	mysqltest.ForEach(t, func(t *testing.T, target mysqltest.Target, db *sql.DB) {
		s := loadGolden(t, target)
		src := NewRowSource(db, s, 0)

		t.Run("single PK", func(t *testing.T) {
			got := fetch(t, src, "orders", []string{"id", "user_id", "ordered_at"}, true,
				collect.Predicate{Columns: []string{"user_id"}, Values: one("1")})
			assertRows(t, got, []collect.Row{
				r("101", "1", "2024-01-01 10:00:00"),
				r("102", "1", "2024-01-02 10:00:00"),
			})
		})

		t.Run("composite PK and 2-column predicate", func(t *testing.T) {
			ships := fetch(t, src, "shipments", []string{"order_id", "seq"}, true,
				collect.Predicate{Columns: []string{"order_id"}, Values: one("101")})
			assertRows(t, ships, []collect.Row{r("101", "1"), r("101", "2")})

			// Feed the fetched composite keys back as the child predicate.
			var vals []collect.Tuple
			for _, row := range ships {
				vals = append(vals, row.Values)
			}
			got := fetch(t, src, "shipment_events", []string{"id", "order_id", "shipment_seq", "status"}, true,
				collect.Predicate{Columns: []string{"order_id", "shipment_seq"}, Values: vals})
			assertRows(t, got, []collect.Row{
				r("1", "101", "1", "shipped"),
				r("2", "101", "1", "delivered"),
				r("3", "101", "2", "shipped"),
			})
		})

		t.Run("PK-less table groups duplicates", func(t *testing.T) {
			cols := []string{"user_id", "order_id", "action", "logged_at"}
			got := fetch(t, src, "audit_logs", cols, false,
				collect.Predicate{Columns: []string{"user_id"}, Values: one("1")})
			assertRows(t, got, []collect.Row{
				rc(2, "1", nil, "login", "2024-01-01 09:00:00"),
				rc(1, "1", nil, "logout", "2024-01-01 18:00:00"),
				rc(1, "1", "101", "order", "2024-01-01 10:00:00"),
				rc(1, "1", "102", "order", "2024-01-02 10:00:00"),
			})

			got = fetch(t, src, "audit_logs", cols, false,
				collect.Predicate{Columns: []string{"order_id"}, Values: one("101")})
			assertRows(t, got, []collect.Row{
				rc(1, "1", "101", "order", "2024-01-01 10:00:00"),
				rc(1, "2", "101", "support", "2024-01-05 12:00:00"),
			})
		})

		t.Run("binary PK and FK", func(t *testing.T) {
			phone := []byte{0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11}
			laptop := []byte{0x00, 0xff, 0x00, 0xff, 0x00, 0xff, 0x00, 0xff, 0x00, 0xff, 0x00, 0xff, 0x00, 0xff, 0x00, 0xff}
			devs := fetch(t, src, "devices", []string{"device_uuid", "user_id", "label"}, true,
				collect.Predicate{Columns: []string{"user_id"}, Values: one("1")})
			assertRows(t, devs, []collect.Row{
				r(phone, "1", "alice-phone"),
				r(laptop, "1", "alice-laptop"),
			})

			var keys []collect.Tuple
			for _, row := range devs {
				if _, ok := row.Values[0].([]byte); !ok {
					t.Fatalf("device_uuid is %T, want []byte", row.Values[0])
				}
				keys = append(keys, collect.Tuple{row.Values[0]})
			}
			got := fetch(t, src, "device_tokens", []string{"id", "device_uuid", "token"}, true,
				collect.Predicate{Columns: []string{"device_uuid"}, Values: keys})
			assertRows(t, got, []collect.Row{
				r("1", phone, []byte{0xa1, 0xa1}),
				r("2", laptop, []byte{0xa2, 0xa2}),
			})

			// The key with 0x00 bytes alone.
			got = fetch(t, src, "device_tokens", []string{"id"}, true,
				collect.Predicate{Columns: []string{"device_uuid"}, Values: one(laptop)})
			assertRows(t, got, []collect.Row{r("2")})
		})

		t.Run("VARBINARY, BLOB, strings and NULL", func(t *testing.T) {
			files := fetch(t, src, "files", []string{"id", "content_hash", "body"}, true,
				collect.Predicate{Columns: []string{"user_id"}, Values: one("1")})
			assertRows(t, files, []collect.Row{r("1", []byte{0xaa, 0x01}, []byte{0x00, 0x01, 0x02, 0x03})})

			got := fetch(t, src, "file_shares", []string{"id", "content_hash", "shared_with"}, true,
				collect.Predicate{Columns: []string{"content_hash"}, Values: []collect.Tuple{{files[0].Values[1]}}})
			assertRows(t, got, []collect.Row{r("1", []byte{0xaa, 0x01}, "bob@example.com")})

			got = fetch(t, src, "folders", []string{"id", "parent_id", "name"}, true,
				collect.Predicate{Columns: []string{"id"}, Values: one("10", "11")})
			assertRows(t, got, []collect.Row{
				r("10", nil, "alice-root"),
				r("11", "10", "alice-docs"),
			})
		})

		t.Run("chunk boundaries", func(t *testing.T) {
			folderIDs := one("10", "11", "12", "13", "20", "21", "999")
			wantFolders := []collect.Row{
				r("10", "1", nil), r("11", "1", "10"), r("12", "1", "11"),
				r("13", "2", "12"), r("20", "2", nil), r("21", "2", "20"),
			}
			pairs := []collect.Tuple{{"101", "1"}, {"101", "2"}, {"201", "1"}, {"999", "1"}}
			wantEvents := []collect.Row{r("1"), r("2"), r("3"), r("4")}
			auditCols := []string{"user_id", "order_id", "action", "logged_at"}
			var wantAudit []collect.Row
			for _, chunk := range []int{1, 2, 0} {
				t.Run(fmt.Sprintf("chunk=%d", chunk), func(t *testing.T) {
					src := NewRowSource(db, s, chunk)
					got := fetch(t, src, "folders", []string{"id", "user_id", "parent_id"}, true,
						collect.Predicate{Columns: []string{"id"}, Values: folderIDs})
					if len(got) != len(wantFolders) {
						t.Errorf("got %d folders, want %d", len(got), len(wantFolders))
					}
					assertRows(t, got, wantFolders)

					got = fetch(t, src, "shipment_events", []string{"id"}, true,
						collect.Predicate{Columns: []string{"order_id", "shipment_seq"}, Values: pairs})
					assertRows(t, got, wantEvents)

					got = fetch(t, src, "audit_logs", auditCols, false,
						collect.Predicate{Columns: []string{"user_id"}, Values: one("1", "2")})
					var total int64
					for _, row := range got {
						total += row.Count
					}
					if total != 8 {
						t.Errorf("audit_logs total count = %d, want 8", total)
					}
					if wantAudit == nil {
						wantAudit = got
					} else {
						assertRows(t, got, wantAudit)
					}
				})
			}
		})

		t.Run("rejects unknown names without a query", func(t *testing.T) {
			cq := &countingQuerier{q: db}
			src := NewRowSource(cq, s, 0)
			ctx := context.Background()
			p := collect.Predicate{Columns: []string{"id"}, Values: one("1")}
			if _, err := src.Fetch(ctx, "no_such_table", []string{"id"}, true, p); !errors.Is(err, ErrUnknownTable) {
				t.Errorf("unknown table: err = %v, want ErrUnknownTable", err)
			}
			// The view exists in the database but not in the schema file.
			if _, err := src.Fetch(ctx, "user_order_totals", []string{"user_id"}, true,
				collect.Predicate{Columns: []string{"user_id"}, Values: one("1")}); !errors.Is(err, ErrUnknownTable) {
				t.Errorf("view: err = %v, want ErrUnknownTable", err)
			}
			if _, err := src.Fetch(ctx, "users", []string{"id", "password"}, true, p); !errors.Is(err, ErrUnknownColumn) {
				t.Errorf("unknown column: err = %v, want ErrUnknownColumn", err)
			}
			if _, err := src.Fetch(ctx, "users", []string{"id"}, true,
				collect.Predicate{Columns: []string{"nope"}, Values: one("1")}); !errors.Is(err, ErrUnknownColumn) {
				t.Errorf("unknown predicate column: err = %v, want ErrUnknownColumn", err)
			}
			if cq.calls != 0 {
				t.Errorf("issued %d queries, want none", cq.calls)
			}
		})
	})
}
