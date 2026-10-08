package relations

import (
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yamagame/mysql-relation-deleter/internal/schema"
)

// testSchema is the schema every testdata file is validated against.
func testSchema() *schema.Schema {
	cols := func(names ...string) []schema.Column {
		out := make([]schema.Column, len(names))
		for i, n := range names {
			out[i] = schema.Column{Name: n, Type: "int"}
		}
		return out
	}
	return &schema.Schema{
		FormatVersion: 1,
		Tables: []schema.Table{
			{Name: "order_items", Columns: cols("order_id", "shop_id", "qty")},
			{Name: "orders", Columns: cols("id", "shop_id", "user_id"), PrimaryKey: []string{"id", "shop_id"}},
			{Name: "users", Columns: cols("id"), PrimaryKey: []string{"id"}},
		},
	}
}

func TestLoadValid(t *testing.T) {
	path := filepath.Join("testdata", "valid.yaml")
	got, err := Load(path, testSchema())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []ManualRelation{
		{
			Name:   "orders_user",
			Child:  Endpoint{Table: "orders", Columns: []string{"user_id"}},
			Parent: Endpoint{Table: "users", Columns: []string{"id"}},
			Line:   3,
		},
		{
			Name:   "manual#1",
			Child:  Endpoint{Table: "order_items", Columns: []string{"order_id", "shop_id"}},
			Parent: Endpoint{Table: "orders", Columns: []string{"id", "shop_id"}},
			Line:   6,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load =\n%#v\nwant\n%#v", got, want)
	}
}

func TestLoadEmptyRelationsIsAllowed(t *testing.T) {
	got, err := Load(filepath.Join("testdata", "empty_relations.yaml"), testSchema())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Load = %v, want no relations", got)
	}
}

func TestLoadSample(t *testing.T) {
	s, err := schema.Load(filepath.Join("..", "..", "testdata", "schema.golden.8.0.json"))
	if err != nil {
		t.Fatalf("schema.Load: %v", err)
	}
	got, err := Load(filepath.Join("..", "..", "testdata", "relations.sample.yaml"), s)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []ManualRelation{{
		Name:   "orders_legacy_user",
		Child:  Endpoint{Table: "legacy_orders", Columns: []string{"customer_id"}},
		Parent: Endpoint{Table: "users", Columns: []string{"id"}},
		Line:   6,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load =\n%#v\nwant\n%#v", got, want)
	}
}

func TestLoadValidationErrors(t *testing.T) {
	type e struct {
		Line    int
		Message string
	}
	// Errors are listed in line order; ties keep detection order.
	tests := []struct {
		file string
		want []e
	}{
		{"unknown_table.yaml", []e{
			{7, `child.table "nope" not found in schema`},
			{11, `parent.table "ghosts" not found in schema`},
		}},
		{"unknown_column.yaml", []e{
			{6, `child.columns[1] "user_idx" not found in table "orders"`},
			{9, `parent.columns[1] "missing" not found in table "users"`},
		}},
		{"count_mismatch.yaml", []e{
			{3, `column count mismatch: child has 2, parent has 1`},
			{7, `child.columns must not be empty`},
			{8, `parent.columns must not be empty`},
		}},
		{"unknown_key.yaml", []e{
			{5, `unknown key "column"`},
			{5, `parent.columns must not be empty`},
			{6, `unknown key "extra"`},
		}},
		{"bad_version.yaml", []e{
			{1, `unsupported version 2 (want 1)`},
		}},
		{"missing_version.yaml", []e{
			{1, `version is required`},
		}},
		{"missing_relations.yaml", []e{
			{1, `relations is required`},
		}},
		{"missing_table.yaml", []e{
			{3, `child.table is required`},
		}},
		{"empty_file.yaml", []e{
			{1, `version is required`},
			{1, `relations is required`},
		}},
		{"multiple_errors.yaml", []e{
			{1, `unsupported version 3 (want 1)`},
			{3, `column count mismatch: child has 2, parent has 1`},
			{4, `child.columns[1] "nope" not found in table "orders"`},
			{7, `child.table "missing" not found in schema`},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			path := filepath.Join("testdata", tt.file)
			got, err := Load(path, testSchema())
			if got != nil {
				t.Errorf("Load returned relations %v, want nil", got)
			}
			var verrs ValidationErrors
			if !errors.As(err, &verrs) {
				t.Fatalf("Load error = %v (%T), want ValidationErrors", err, err)
			}
			var gotE []e
			for _, ve := range verrs {
				if ve.Path != path {
					t.Errorf("Path = %q, want %q", ve.Path, path)
				}
				gotE = append(gotE, e{ve.Line, ve.Message})
			}
			if !reflect.DeepEqual(gotE, tt.want) {
				t.Fatalf("errors =\n%v\nwant\n%v", gotE, tt.want)
			}
			// Every entry must appear in the combined message with file:line.
			msg := err.Error()
			for _, w := range tt.want {
				if !strings.Contains(msg, path+":") || !strings.Contains(msg, w.Message) {
					t.Errorf("Error() = %q, missing %q", msg, w.Message)
				}
			}
		})
	}
}

func TestValidationErrorsFormat(t *testing.T) {
	err := ValidationErrors{
		{Path: "r.yaml", Line: 3, Message: "a"},
		{Path: "r.yaml", Line: 7, Message: "b"},
	}
	want := "r.yaml:3: a\nr.yaml:7: b"
	if got := err.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if got := err[0].Error(); got != "r.yaml:3: a" {
		t.Fatalf("ValidationError.Error() = %q", got)
	}
}

func TestLoadMissingFileWrapsCause(t *testing.T) {
	path := filepath.Join("testdata", "does_not_exist.yaml")
	_, err := Load(path, testSchema())
	if err == nil {
		t.Fatal("Load: want error")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error %v does not wrap fs.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not contain path", err)
	}
}

func TestLoadMalformedYAML(t *testing.T) {
	path := filepath.Join("testdata", "malformed.yaml")
	_, err := Load(path, testSchema())
	if err == nil {
		t.Fatal("Load: want error")
	}
	var verrs ValidationErrors
	if errors.As(err, &verrs) {
		t.Fatalf("malformed YAML returned ValidationErrors %v, want a syntax error", verrs)
	}
	if errors.Unwrap(err) == nil {
		t.Errorf("error %v does not wrap its cause", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not contain path", err)
	}
}

func TestLoadNilSchema(t *testing.T) {
	if _, err := Load(filepath.Join("testdata", "valid.yaml"), nil); err == nil {
		t.Fatal("Load with nil schema: want error")
	}
}
