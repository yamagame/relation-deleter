//go:build integration

// Package integration holds the end-to-end tests that run against the Docker
// MySQL 5.7 and 8.0 servers defined in testdata/integration/docker-compose.yml.
package integration

import (
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/yamagame/relation-deleter/internal/testutil/mysqltest"
)

// fixtureTables lists the base tables that testdata/integration/fixture.sql creates.
var fixtureTables = []string{
	"audit_logs",
	"device_tokens",
	"devices",
	"file_shares",
	"files",
	"folders",
	"legacy_orders",
	"newsletter_subscriptions",
	"order_items",
	"orders",
	"shipment_events",
	"shipments",
	"subscription_deliveries",
	"team_members",
	"teams",
	"users",
}

// fixtureViews lists the views that the fixture creates.
var fixtureViews = []string{"user_order_totals"}

func TestFixtureLoaded(t *testing.T) {
	mysqltest.ForEach(t, func(t *testing.T, target mysqltest.Target, db *sql.DB) {
		var version string
		if err := db.QueryRow("SELECT VERSION()").Scan(&version); err != nil {
			t.Fatalf("SELECT VERSION(): %v", err)
		}
		t.Logf("server version: %s", version)
		if !strings.HasPrefix(version, target.Name+".") {
			t.Errorf("server version %q does not match target %q", version, target.Name)
		}

		if got := listTables(t, db, target.DB, "BASE TABLE"); !slices.Equal(got, fixtureTables) {
			t.Errorf("base tables = %v, want %v", got, fixtureTables)
		}
		if got := listTables(t, db, target.DB, "VIEW"); !slices.Equal(got, fixtureViews) {
			t.Errorf("views = %v, want %v", got, fixtureViews)
		}

		// users.id = 1 is the intended deletion root; rows reachable only from
		// users.id = 2 must survive (see the fixture.sql header).
		for _, id := range []int{1, 2} {
			var n int
			if err := db.QueryRow("SELECT COUNT(*) FROM users WHERE id = ?", id).Scan(&n); err != nil {
				t.Fatalf("count users id=%d: %v", id, err)
			}
			if n != 1 {
				t.Errorf("users id=%d: got %d rows, want 1", id, n)
			}
		}
	})
}

func listTables(t *testing.T, db *sql.DB, schema, tableType string) []string {
	t.Helper()
	rows, err := db.Query(
		"SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = ?",
		schema, tableType)
	if err != nil {
		t.Fatalf("list %s: %v", tableType, err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan %s: %v", tableType, err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list %s: %v", tableType, err)
	}
	slices.Sort(names)
	return names
}
