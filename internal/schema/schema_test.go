package schema

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadValid(t *testing.T) {
	s, err := Load(filepath.Join("testdata", "valid.json"))
	if err != nil {
		t.Fatalf("Load(valid.json) returned error: %v", err)
	}
	if s == nil {
		t.Fatal("Load(valid.json) returned nil schema")
	}
	if s.FormatVersion != 1 || s.Database != "app" || s.ServerVersion != "5.7.44" {
		t.Errorf("header = (%d, %q, %q), want (1, \"app\", \"5.7.44\")", s.FormatVersion, s.Database, s.ServerVersion)
	}
	if len(s.Tables) != 4 || len(s.ForeignKeys) != 3 {
		t.Fatalf("got %d tables / %d FKs, want 4 / 3", len(s.Tables), len(s.ForeignKeys))
	}

	items, ok := s.Table("order_items")
	if !ok {
		t.Fatal("Table(order_items) not found")
	}
	wantCols := []string{"order_id", "line_no", "sku"}
	for i, c := range items.Columns {
		if c.Name != wantCols[i] {
			t.Errorf("order_items column %d = %q, want %q (ordinal order must be kept)", i, c.Name, wantCols[i])
		}
	}
	if got := strings.Join(items.PrimaryKey, ","); got != "order_id,line_no" {
		t.Errorf("order_items PK = %q, want order_id,line_no", got)
	}

	orders, _ := s.Table("orders")
	col, ok := orders.Column("user_id")
	if !ok || col.Type != "binary(16)" || !col.Nullable || !col.IsBinary() {
		t.Errorf("orders.user_id = %+v (found=%v), want nullable binary(16)", col, ok)
	}
	if _, ok := orders.Column("missing"); ok {
		t.Error("Column(missing) reported found")
	}
	if _, ok := s.Table("missing"); ok {
		t.Error("Table(missing) reported found")
	}

	// Table/Column return pointers into the schema itself.
	tp, _ := s.Table("users")
	if tp != &s.Tables[3] {
		t.Error("Table(users) did not return a pointer into Schema.Tables")
	}

	fk := s.ForeignKeys[2]
	if fk.Name != "fk_shipment_lines_item" || fk.DeleteRule != "NO ACTION" ||
		strings.Join(fk.Columns, ",") != "order_id,line_no" ||
		strings.Join(fk.ReferencedColumns, ",") != "order_id,line_no" {
		t.Errorf("composite FK decoded incorrectly: %+v", fk)
	}
	shipment, _ := s.Table("shipment_lines")
	if len(shipment.PrimaryKey) != 0 {
		t.Errorf("shipment_lines PK = %v, want empty", shipment.PrimaryKey)
	}
}

func TestLoadInvalidFiles(t *testing.T) {
	tests := []struct {
		name string
		file string
		want []string // substrings that must appear in the error
	}{
		{"missing file", "does_not_exist.json", []string{"does_not_exist.json", "read"}},
		{"malformed JSON", "invalid_json.json", []string{"invalid_json.json", "invalid JSON"}},
		{"unsupported format_version", "invalid_version.json", []string{"format_version", "2"}},
		{"multiple structural problems", "invalid_structure.json", []string{
			`duplicate table name "users"`,
			`duplicate column name "id" in table "users"`,
			`empty table name`,
			`primary key column "uid" does not exist in table "users"`,
			`foreign key "fk_orders_user": column "user_ref" does not exist in table "orders"`,
			`foreign key "fk_orders_user": referenced column "user_id" does not exist in table "users"`,
			`foreign key "fk_orders_ghost": referenced table "ghosts" does not exist`,
			`foreign key "fk_count": column count mismatch (2 vs 1)`,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join("testdata", tt.file)
			s, err := Load(path)
			if err == nil {
				t.Fatalf("Load(%s) = %+v, nil; want error", tt.file, s)
			}
			if s != nil {
				t.Errorf("Load(%s) returned non-nil schema with error", tt.file)
			}
			var le *LoadError
			if !errors.As(err, &le) {
				t.Fatalf("error %T is not *LoadError: %v", err, err)
			}
			if le.Path != path {
				t.Errorf("LoadError.Path = %q, want %q", le.Path, path)
			}
			if le.Reason == "" {
				t.Error("LoadError.Reason is empty")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err.Error(), w)
				}
			}
		})
	}
}

func TestLoadMissingFileWrapsCause(t *testing.T) {
	_, err := Load(filepath.Join("testdata", "does_not_exist.json"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("errors.Is(err, fs.ErrNotExist) = false for %v", err)
	}
}

// base returns a minimal valid schema that each inline case mutates.
func base() Schema {
	return Schema{
		FormatVersion: 1,
		Database:      "app",
		ServerVersion: "8.0.36",
		Tables: []Table{
			{Name: "orders", Columns: []Column{{Name: "id", Type: "bigint"}, {Name: "user_id", Type: "bigint", Nullable: true}}, PrimaryKey: []string{"id"}},
			{Name: "users", Columns: []Column{{Name: "id", Type: "bigint"}}, PrimaryKey: []string{"id"}},
		},
		ForeignKeys: []ForeignKey{
			{Name: "fk_orders_user", Table: "orders", Columns: []string{"user_id"}, ReferencedTable: "users", ReferencedColumns: []string{"id"}, DeleteRule: "CASCADE"},
		},
	}
}

func writeSchema(t *testing.T, s Schema) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValidationCases(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(s *Schema)
		want   []string // empty means the schema must load successfully
	}{
		{"base is valid", func(s *Schema) {}, nil},
		{"format_version 0", func(s *Schema) { s.FormatVersion = 0 }, []string{"unsupported format_version 0"}},
		{"empty table name", func(s *Schema) { s.Tables[1].Name = "" }, []string{"empty table name"}},
		{"duplicate table", func(s *Schema) { s.Tables[1].Name = "orders" }, []string{`duplicate table name "orders"`}},
		{"empty column name", func(s *Schema) { s.Tables[0].Columns[1].Name = "" }, []string{`empty column name in table "orders"`}},
		{"duplicate column", func(s *Schema) { s.Tables[0].Columns[1].Name = "id" }, []string{`duplicate column name "id" in table "orders"`}},
		{"missing PK column", func(s *Schema) { s.Tables[1].PrimaryKey = []string{"uid"} }, []string{`primary key column "uid" does not exist in table "users"`}},
		{"unknown FK table", func(s *Schema) { s.ForeignKeys[0].Table = "nope" }, []string{`foreign key "fk_orders_user": table "nope" does not exist`}},
		{"unknown referenced table", func(s *Schema) { s.ForeignKeys[0].ReferencedTable = "nope" }, []string{`foreign key "fk_orders_user": referenced table "nope" does not exist`}},
		{"missing FK column", func(s *Schema) { s.ForeignKeys[0].Columns = []string{"x"} }, []string{`foreign key "fk_orders_user": column "x" does not exist in table "orders"`}},
		{"missing referenced column", func(s *Schema) { s.ForeignKeys[0].ReferencedColumns = []string{"y"} }, []string{`foreign key "fk_orders_user": referenced column "y" does not exist in table "users"`}},
		{"FK count mismatch", func(s *Schema) { s.ForeignKeys[0].ReferencedColumns = []string{"id", "id"} }, []string{`foreign key "fk_orders_user": column count mismatch (1 vs 2)`}},
		{"FK zero columns", func(s *Schema) {
			s.ForeignKeys[0].Columns = nil
			s.ForeignKeys[0].ReferencedColumns = nil
		}, []string{`foreign key "fk_orders_user": has no columns`}},
		{"all problems are collected", func(s *Schema) {
			s.FormatVersion = 3
			s.Tables[0].Columns[1].Name = "id"
			s.ForeignKeys[0].ReferencedTable = "nope"
		}, []string{"unsupported format_version 3", `duplicate column name "id" in table "orders"`, `referenced table "nope" does not exist`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := base()
			tt.mutate(&s)
			path := writeSchema(t, s)
			got, err := Load(path)
			if len(tt.want) == 0 {
				if err != nil || got == nil {
					t.Fatalf("Load() = %v, %v; want schema, nil", got, err)
				}
				return
			}
			var le *LoadError
			if !errors.As(err, &le) {
				t.Fatalf("Load() error = %v (%T); want *LoadError", err, err)
			}
			if got != nil {
				t.Error("Load() returned non-nil schema with error")
			}
			for _, w := range tt.want {
				if !strings.Contains(le.Reason, w) {
					t.Errorf("Reason %q does not contain %q", le.Reason, w)
				}
			}
		})
	}
}

func TestLoadErrorMessage(t *testing.T) {
	e := &LoadError{Path: "s.json", Reason: "bad"}
	if got, want := e.Error(), "schema file s.json: bad"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestIsBinary(t *testing.T) {
	tests := []struct {
		typ  string
		want bool
	}{
		{"binary(16)", true},
		{"BINARY(16)", true},
		{"varbinary(255)", true},
		{"VarBinary(8)", true},
		{"tinyblob", true},
		{"blob", true},
		{"mediumblob", true},
		{"LONGBLOB", true},
		{"bit(1)", true},
		{"bit(64)", true},
		{" binary(4) ", true},
		{"char(16)", false},
		{"varchar(255)", false},
		{"varchar", false},
		{"json", false},
		{"text", false},
		{"mediumtext", false},
		{"int(11)", false},
		{"int", false},
		{"bigint unsigned", false},
		{"bigint(20) unsigned", false},
		{"enum('binary','blob')", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.typ, func(t *testing.T) {
			if got := (Column{Type: tt.typ}).IsBinary(); got != tt.want {
				t.Errorf("Column{Type: %q}.IsBinary() = %v, want %v", tt.typ, got, tt.want)
			}
		})
	}
}

func TestQuoteIdent(t *testing.T) {
	tests := []struct{ in, want string }{
		{"orders", "`orders`"},
		{"order items", "`order items`"},
		{"a`b", "`a``b`"},
		{"``", "``````"},
		{"", "``"},
		{"`; DROP TABLE x; --", "```; DROP TABLE x; --`"},
	}
	for _, tt := range tests {
		if got := QuoteIdent(tt.in); got != tt.want {
			t.Errorf("QuoteIdent(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
