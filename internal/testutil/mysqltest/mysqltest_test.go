//go:build integration

package mysqltest

import (
	"database/sql"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestTargets(t *testing.T) {
	tests := []struct {
		env     string
		want    []string
		wantErr bool
	}{
		{env: "", want: []string{"5.7", "8.0"}},
		{env: "8.0", want: []string{"8.0"}},
		{env: " 5.7 ", want: []string{"5.7"}},
		{env: "8.0,5.7", want: []string{"8.0", "5.7"}},
		{env: "5.6", wantErr: true},
		{env: ",", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			t.Setenv(VersionsEnv, tt.env)
			got, err := Targets()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Targets() = %v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, tg := range got {
				names = append(names, tg.Name)
			}
			if !slices.Equal(names, tt.want) {
				t.Errorf("Targets() = %v, want %v", names, tt.want)
			}
		})
	}
}

func TestIsolatedName(t *testing.T) {
	long := strings.Repeat("VeryLongTestName/", 10)
	for _, name := range []string{"TestX/mysql8.0", long} {
		got := isolatedName(name)
		if len(got) > maxIdentLen {
			t.Errorf("isolatedName(%q) = %q: %d bytes, want <= %d", name, got, len(got), maxIdentLen)
		}
		if !regexp.MustCompile(`^app_t_[a-z0-9_]*_[0-9a-f]{8}$`).MatchString(got) {
			t.Errorf("isolatedName(%q) = %q: unexpected form", name, got)
		}
		if again := isolatedName(name); again == got {
			t.Errorf("isolatedName(%q) returned %q twice", name, got)
		}
	}
	if got := isolatedName("TestX/mysql8.0"); !strings.HasPrefix(got, "app_t_testx_mysql8_0_") {
		t.Errorf("isolatedName = %q, want prefix app_t_testx_mysql8_0_", got)
	}
}

// TestIsolatedDB checks that an isolated database holds the fixture, that
// changing it leaves the shared database alone, that ResetFixture restores it,
// and that it is dropped when the test ends.
func TestIsolatedDB(t *testing.T) {
	ForEach(t, func(t *testing.T, target Target, shared *sql.DB) {
		count := func(db *sql.DB) int {
			t.Helper()
			var n int
			if err := db.QueryRow("SELECT COUNT(*) FROM order_items").Scan(&n); err != nil {
				t.Fatalf("count order_items: %v", err)
			}
			return n
		}
		sharedBefore := count(shared)
		if sharedBefore == 0 {
			t.Fatal("shared fixture has no order_items rows")
		}

		var isolatedName string
		t.Run("isolated", func(t *testing.T) {
			isolated := IsolatedDB(t, target)
			isolatedName = isolated.DB
			if isolated.DB == SharedDB {
				t.Fatalf("IsolatedDB returned the shared database")
			}
			db, err := isolated.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			if n := count(db); n != sharedBefore {
				t.Fatalf("isolated order_items = %d, want %d", n, sharedBefore)
			}
			if _, err := db.Exec("DELETE FROM order_items"); err != nil {
				t.Fatalf("delete order_items: %v", err)
			}
			if n := count(db); n != 0 {
				t.Fatalf("isolated order_items after delete = %d, want 0", n)
			}
			if n := count(shared); n != sharedBefore {
				t.Errorf("shared order_items after isolated delete = %d, want %d", n, sharedBefore)
			}

			ResetFixture(t, isolated)
			if n := count(db); n != sharedBefore {
				t.Errorf("isolated order_items after reset = %d, want %d", n, sharedBefore)
			}
		})

		var n int
		if err := shared.QueryRow(
			"SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", isolatedName,
		).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("isolated database %s still exists after the test", isolatedName)
		}
	})
}
