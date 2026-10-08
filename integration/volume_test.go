//go:build integration

package integration

import (
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yamagame/relation-deleter/internal/testutil/mysqltest"
)

// The large-volume root: a user with volumeOrders orders and nothing else
// (8.1, 8.2). Its ids are outside the fixture's ranges.
const (
	volumeUserID       = 900
	volumeOrders       = 50000
	volumeFirstOrderID = 1000000
	volumeInsertBatch  = 2000 // rows per multi-row INSERT (3 placeholders each)
	volumeTimeout      = 10 * time.Minute

	// volumeTotal is the user plus every order; volumeTables is users and
	// orders.
	volumeTotal  = 1 + volumeOrders
	volumeTables = 2

	// shrinkOrders exceeds the 65,535 placeholder limit, so that
	// --chunk-size 100000 must be split into a 65,535-value DELETE and the
	// rest; with volumeOrders (below the limit) one DELETE takes them all
	// whether or not the chunk is shrunk.
	shrinkOrders     = 70000
	placeholderLimit = 65535
)

// seedVolume inserts the large-volume user and its orders orders into target
// in one transaction, with batched multi-row INSERTs.
func seedVolume(t *testing.T, target mysqltest.Target, orders int) {
	t.Helper()
	db, err := target.Open()
	if err != nil {
		t.Fatalf("open %s: %v", target.DB, err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO users (id, email, name) VALUES (?, ?, ?)",
		volumeUserID, "volume@example.com", "volume"); err != nil {
		t.Fatalf("insert user %d: %v", volumeUserID, err)
	}
	for start := 0; start < orders; start += volumeInsertBatch {
		n := min(volumeInsertBatch, orders-start)
		var q strings.Builder
		q.WriteString("INSERT INTO orders (id, user_id, ordered_at) VALUES ")
		args := make([]any, 0, 3*n)
		for i := range n {
			if i > 0 {
				q.WriteByte(',')
			}
			q.WriteString("(?,?,?)")
			args = append(args, volumeFirstOrderID+start+i, volumeUserID, "2024-06-01 00:00:00")
		}
		if _, err := tx.Exec(q.String(), args...); err != nil {
			t.Fatalf("insert orders from %d: %v", volumeFirstOrderID+start, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// countRows returns SELECT COUNT(*) FROM <from> [WHERE ...] on target.
func countRows(t *testing.T, target mysqltest.Target, query string, args ...any) int {
	t.Helper()
	db, err := target.Open()
	if err != nil {
		t.Fatalf("open %s: %v", target.DB, err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// volumeCounts is the state the test checks before and after a run.
type volumeCounts struct {
	users, orders, volumeOrders, volumeUser int
}

func takeVolumeCounts(t *testing.T, target mysqltest.Target) volumeCounts {
	t.Helper()
	return volumeCounts{
		users:        countRows(t, target, "SELECT COUNT(*) FROM users"),
		orders:       countRows(t, target, "SELECT COUNT(*) FROM orders"),
		volumeOrders: countRows(t, target, "SELECT COUNT(*) FROM orders WHERE user_id = ?", volumeUserID),
		volumeUser:   countRows(t, target, "SELECT COUNT(*) FROM users WHERE id = ?", volumeUserID),
	}
}

// progressLineRE matches a report.Progress line on stderr.
var progressLineRE = regexp.MustCompile(`^(collect|delete) ([a-z_]+): ([0-9]+) rows$`)

// progressCounts returns the counts of the progress lines of phase and table
// in stderr, in order, and the number of progress lines of any kind.
func progressCounts(stderr, phase, table string) (counts []int64, lines int) {
	for _, line := range strings.Split(stderr, "\n") {
		m := progressLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		lines++
		if m[1] == phase && m[2] == table {
			n, _ := strconv.ParseInt(m[3], 10, 64)
			counts = append(counts, n)
		}
	}
	return counts, lines
}

// assertThrottled checks that the progress lines of one table end at want,
// increase, and are throttled: after the first line, each one crosses a new
// multiple of 1,000 (report.Progress).
func assertThrottled(t *testing.T, label string, counts []int64, want int64) {
	t.Helper()
	if len(counts) == 0 {
		t.Errorf("%s: no progress lines", label)
		return
	}
	if last := counts[len(counts)-1]; last != want {
		t.Errorf("%s: last progress count = %d, want %d (lines %v)", label, last, want, counts)
	}
	for i := 1; i < len(counts); i++ {
		if counts[i]/1000 <= counts[i-1]/1000 {
			t.Errorf("%s: progress line %d (%d rows) does not cross a new 1,000 after %d", label, i, counts[i], counts[i-1])
		}
	}
	if maxLines := int(want/1000) + 1; len(counts) > maxLines {
		t.Errorf("%s: %d progress lines, want at most %d", label, len(counts), maxLines)
	}
}

// TestDeleteLargeVolume deletes a user with 50,000 orders (8.1, 8.2): the
// dry-run and the execute complete without statement-size or placeholder
// errors, show progress on stderr, and delete exactly 50,001 rows. The
// oversized --chunk-size 100000 forces the automatic shrink to the 65,535
// placeholder limit against a real server; the default chunk (500) is run on
// a second database.
func TestDeleteLargeVolume(t *testing.T) {
	mysqltest.ForEach(t, func(t *testing.T, shared mysqltest.Target, _ *sql.DB) {
		// setup returns a fresh database with the fixture plus the volume
		// user, its dumped schema and the fixture's own counts.
		setup := func(t *testing.T, orders int) (mysqltest.Target, string, volumeCounts) {
			target := mysqltest.IsolatedDB(t, shared)
			fixture := takeVolumeCounts(t, target)
			start := time.Now()
			seedVolume(t, target, orders)
			t.Logf("MySQL %s: seeded %d orders in %v", shared.Name, orders, time.Since(start).Round(time.Millisecond))
			schemaPath := dumpSchema(t, target)
			return target, schemaPath, fixture
		}

		// checkDeleted checks that exactly the volume user and its orders
		// are gone and the result reports them.
		checkDeleted := func(t *testing.T, target mysqltest.Target, fixture volumeCounts, res cliResult, orders int) {
			t.Helper()
			if got := takeVolumeCounts(t, target); got != (volumeCounts{users: fixture.users, orders: fixture.orders}) {
				t.Errorf("counts after execute = %+v, want users=%d orders=%d and no volume rows", got, fixture.users, fixture.orders)
			}
			if got, want := deletedCounts(t, res.out), map[string]int{"users": 1, "orders": orders}; got["users"] != want["users"] || got["orders"] != want["orders"] || len(got) != len(want) {
				t.Errorf("deleted counts = %v, want %v", got, want)
			}
			if footer := fmt.Sprintf("total=%d tables=%d\n", 1+orders, volumeTables); !strings.HasSuffix(res.out, footer) {
				t.Errorf("result does not end with %q:\n%s", footer, tail(res.out))
			}
		}

		t.Run("dry-run_then_execute_chunk_100000", func(t *testing.T) {
			target, schemaPath, fixture := setup(t, volumeOrders)
			seeded := takeVolumeCounts(t, target)
			if want := (volumeCounts{users: fixture.users + 1, orders: fixture.orders + volumeOrders, volumeOrders: volumeOrders, volumeUser: 1}); seeded != want {
				t.Fatalf("seeded counts = %+v, want %+v", seeded, want)
			}

			// (a) Dry-run with the default chunk: the plan of 50,001 rows,
			// collect progress on stderr, and no change.
			start := time.Now()
			dry := runDeleterTimeout(t, volumeTimeout, target, schemaPath, "users", strconv.Itoa(volumeUserID))
			t.Logf("MySQL %s: dry-run (chunk 500) took %v", shared.Name, time.Since(start).Round(time.Millisecond))
			if dry.code != 0 {
				t.Fatalf("dry-run exit code = %d, want 0; stderr:\n%s", dry.code, tail(dry.err))
			}
			if footer := fmt.Sprintf("total=%d tables=%d\n", volumeTotal, volumeTables); !strings.HasSuffix(dry.out, footer) {
				t.Errorf("plan does not end with %q:\n%s", footer, tail(dry.out))
			}
			for _, s := range []string{"users: 1 row", fmt.Sprintf("orders: %d rows", volumeOrders)} {
				if !strings.Contains(dry.out, s) {
					t.Errorf("plan does not contain %q", s)
				}
			}
			collected, lines := progressCounts(dry.err, "collect", "orders")
			assertThrottled(t, "dry-run collect orders", collected, volumeOrders)
			if users, _ := progressCounts(dry.err, "collect", "users"); len(users) == 0 {
				t.Errorf("dry-run: no collect progress for users:\n%s", tail(dry.err))
			}
			if lines == 0 || lines > 1000 {
				t.Errorf("dry-run: %d progress lines, want between 1 and 1000 (throttled)", lines)
			}
			t.Logf("MySQL %s: dry-run progress lines = %d, collect orders = %v", shared.Name, lines, collected)
			if got := takeVolumeCounts(t, target); got != seeded {
				t.Errorf("dry-run changed the data: counts = %+v, want %+v", got, seeded)
			}

			// (b) Execute with an oversized chunk (shrunk to 65,535
			// single-column placeholders): every order goes in one DELETE
			// and one SELECT of each child table.
			start = time.Now()
			res := runDeleterTimeout(t, volumeTimeout, target, schemaPath, "users", strconv.Itoa(volumeUserID),
				"--execute", "--yes", "--chunk-size", "100000")
			t.Logf("MySQL %s: execute (chunk 100000) took %v", shared.Name, time.Since(start).Round(time.Millisecond))
			if res.code != 0 {
				t.Fatalf("execute exit code = %d, want 0; stdout:\n%s\nstderr:\n%s", res.code, tail(res.out), tail(res.err))
			}
			checkDeleted(t, target, fixture, res, volumeOrders)
			deleted, lines := progressCounts(res.err, "delete", "orders")
			assertThrottled(t, "execute (chunk 100000) delete orders", deleted, volumeOrders)
			if users, _ := progressCounts(res.err, "delete", "users"); len(users) == 0 {
				t.Errorf("execute: no delete progress for users:\n%s", tail(res.err))
			}
			t.Logf("MySQL %s: execute progress lines = %d, delete orders = %v", shared.Name, lines, deleted)
		})

		// (c) Execute with the default chunk: 100 DELETEs of orders, whose
		// progress is throttled to every 1,000 rows.
		t.Run("execute_default_chunk", func(t *testing.T) {
			target, schemaPath, fixture := setup(t, volumeOrders)
			start := time.Now()
			res := runDeleterTimeout(t, volumeTimeout, target, schemaPath, "users", strconv.Itoa(volumeUserID),
				"--execute", "--yes")
			t.Logf("MySQL %s: execute (chunk 500) took %v", shared.Name, time.Since(start).Round(time.Millisecond))
			if res.code != 0 {
				t.Fatalf("execute exit code = %d, want 0; stdout:\n%s\nstderr:\n%s", res.code, tail(res.out), tail(res.err))
			}
			checkDeleted(t, target, fixture, res, volumeOrders)
			deleted, lines := progressCounts(res.err, "delete", "orders")
			assertThrottled(t, "execute (chunk 500) delete orders", deleted, volumeOrders)
			if len(deleted) < 2 {
				t.Errorf("execute (chunk 500): delete orders progress = %v, want several throttled lines", deleted)
			}
			t.Logf("MySQL %s: execute progress lines = %d, delete orders lines = %d", shared.Name, lines, len(deleted))
		})

		// (d) Above the placeholder limit: --chunk-size 100000 is shrunk to
		// 65,535, so the orders are deleted by a 65,535-value DELETE and a
		// 4,465-value one, without a placeholder-limit error (1390).
		t.Run("execute_chunk_shrunk_to_placeholder_limit", func(t *testing.T) {
			target, schemaPath, fixture := setup(t, shrinkOrders)
			start := time.Now()
			res := runDeleterTimeout(t, volumeTimeout, target, schemaPath, "users", strconv.Itoa(volumeUserID),
				"--execute", "--yes", "--chunk-size", "100000")
			t.Logf("MySQL %s: execute of %d orders (chunk 100000 -> %d) took %v", shared.Name, shrinkOrders, placeholderLimit, time.Since(start).Round(time.Millisecond))
			if res.code != 0 {
				t.Fatalf("execute exit code = %d, want 0; stdout:\n%s\nstderr:\n%s", res.code, tail(res.out), tail(res.err))
			}
			checkDeleted(t, target, fixture, res, shrinkOrders)
			deleted, _ := progressCounts(res.err, "delete", "orders")
			if want := []int64{placeholderLimit, shrinkOrders}; !slices.Equal(deleted, want) {
				t.Errorf("delete orders progress = %v, want %v (one DELETE per %d values)", deleted, want, placeholderLimit)
			}
		})
	})
}

// tail returns the end of s (at most 40 lines and 4 KiB), so a failure does
// not print the whole 50,000-row plan.
func tail(s string) string {
	const maxLines, maxBytes = 40, 4096
	out := s
	if lines := strings.Split(out, "\n"); len(lines) > maxLines {
		out = strings.Join(lines[len(lines)-maxLines:], "\n")
	}
	if len(out) > maxBytes {
		out = out[len(out)-maxBytes:]
	}
	if len(out) < len(s) {
		return fmt.Sprintf("... (%d bytes omitted)\n%s", len(s)-len(out), out)
	}
	return s
}
