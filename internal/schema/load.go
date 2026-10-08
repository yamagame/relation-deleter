package schema

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// SupportedFormatVersion is the only schema file format this package reads.
const SupportedFormatVersion = 1

// LoadError reports why a schema file could not be loaded. Reason lists every
// problem found, separated by "; ". Err holds the underlying cause for read
// and JSON errors (nil for validation errors).
type LoadError struct {
	Path   string
	Reason string
	Err    error
}

func (e *LoadError) Error() string {
	return fmt.Sprintf("schema file %s: %s", e.Path, e.Reason)
}

func (e *LoadError) Unwrap() error { return e.Err }

// Load reads the schema file at path and validates its structure. On any
// problem it returns a *LoadError and a nil schema.
//
// Unknown JSON fields are deliberately ignored (encoding/json's default) so
// that a newer dump script may add fields without breaking older readers;
// only the fields defined here are validated.
func Load(path string) (*Schema, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &LoadError{Path: path, Reason: fmt.Sprintf("cannot read file: %v", err), Err: err}
	}
	var s Schema
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, &LoadError{Path: path, Reason: fmt.Sprintf("invalid JSON: %v", err), Err: err}
	}
	if problems := s.validate(); len(problems) > 0 {
		return nil, &LoadError{Path: path, Reason: strings.Join(problems, "; ")}
	}
	return &s, nil
}

// validate returns every structural problem in s, in file order.
func (s *Schema) validate() []string {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if s.FormatVersion != SupportedFormatVersion {
		add("unsupported format_version %d (want %d)", s.FormatVersion, SupportedFormatVersion)
	}

	seenTables := make(map[string]bool, len(s.Tables))
	for i := range s.Tables {
		t := &s.Tables[i]
		switch {
		case t.Name == "":
			add("empty table name (tables[%d])", i)
		case seenTables[t.Name]:
			add("duplicate table name %q", t.Name)
		default:
			seenTables[t.Name] = true
		}

		seenCols := make(map[string]bool, len(t.Columns))
		for _, c := range t.Columns {
			switch {
			case c.Name == "":
				add("empty column name in table %q", t.Name)
			case seenCols[c.Name]:
				add("duplicate column name %q in table %q", c.Name, t.Name)
			default:
				seenCols[c.Name] = true
			}
		}
		for _, pk := range t.PrimaryKey {
			if !seenCols[pk] {
				add("primary key column %q does not exist in table %q", pk, t.Name)
			}
		}
	}

	for _, fk := range s.ForeignKeys {
		child, childOK := s.Table(fk.Table)
		if !childOK {
			add("foreign key %q: table %q does not exist", fk.Name, fk.Table)
		}
		parent, parentOK := s.Table(fk.ReferencedTable)
		if !parentOK {
			add("foreign key %q: referenced table %q does not exist", fk.Name, fk.ReferencedTable)
		}
		if len(fk.Columns) == 0 || len(fk.ReferencedColumns) == 0 {
			add("foreign key %q: has no columns", fk.Name)
		} else if len(fk.Columns) != len(fk.ReferencedColumns) {
			add("foreign key %q: column count mismatch (%d vs %d)", fk.Name, len(fk.Columns), len(fk.ReferencedColumns))
		}
		if childOK {
			for _, c := range fk.Columns {
				if _, ok := child.Column(c); !ok {
					add("foreign key %q: column %q does not exist in table %q", fk.Name, c, fk.Table)
				}
			}
		}
		if parentOK {
			for _, c := range fk.ReferencedColumns {
				if _, ok := parent.Column(c); !ok {
					add("foreign key %q: referenced column %q does not exist in table %q", fk.Name, c, fk.ReferencedTable)
				}
			}
		}
	}
	return problems
}
