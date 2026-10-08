// Package schema holds the typed form of the schema file produced by
// scripts/dump-schema.sh (format_version 1), together with its loader and
// structural validation. It is the lowest layer of the module and depends on
// the standard library only.
package schema

import "strings"

// Column is one table column. Type is the raw INFORMATION_SCHEMA.COLUMN_TYPE
// value, which differs between MySQL versions (e.g. "int(11)" on 5.7 and
// "int" on 8.0.19+), so callers must only rely on the leading type name.
type Column struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // COLUMN_TYPE (e.g. "bigint unsigned", "binary(16)")
	Nullable bool   `json:"nullable"`
}

// Table is one base table. Columns are in ORDINAL_POSITION order.
type Table struct {
	Name       string   `json:"name"`
	Columns    []Column `json:"columns"`     // ORDINAL_POSITION order
	PrimaryKey []string `json:"primary_key"` // empty means the table has no PK
}

// ForeignKey is a foreign key whose child and parent are in the same database.
// Columns[i] references ReferencedColumns[i].
type ForeignKey struct {
	Name              string   `json:"name"`
	Table             string   `json:"table"`
	Columns           []string `json:"columns"`
	ReferencedTable   string   `json:"referenced_table"`
	ReferencedColumns []string `json:"referenced_columns"`
	DeleteRule        string   `json:"delete_rule"` // display only; never drives behavior (4.2)
}

// Schema is the whole schema file.
type Schema struct {
	FormatVersion int          `json:"format_version"` // must be 1
	Database      string       `json:"database"`
	ServerVersion string       `json:"server_version"`
	Tables        []Table      `json:"tables"`
	ForeignKeys   []ForeignKey `json:"foreign_keys"`
}

// Table returns the table with the given name. The returned pointer refers to
// the element inside s.Tables.
func (s *Schema) Table(name string) (*Table, bool) {
	for i := range s.Tables {
		if s.Tables[i].Name == name {
			return &s.Tables[i], true
		}
	}
	return nil, false
}

// Column returns the column with the given name. The returned pointer refers
// to the element inside t.Columns.
func (t *Table) Column(name string) (*Column, bool) {
	for i := range t.Columns {
		if t.Columns[i].Name == name {
			return &t.Columns[i], true
		}
	}
	return nil, false
}

// binaryTypes are the MySQL type names whose values are raw bytes.
var binaryTypes = map[string]bool{
	"binary":     true,
	"varbinary":  true,
	"tinyblob":   true,
	"blob":       true,
	"mediumblob": true,
	"longblob":   true,
	"bit":        true,
}

// IsBinary reports whether the column holds raw bytes (binary, varbinary,
// *blob, bit). It looks only at the leading type name, case-insensitively, so
// it works for both the 5.7 and 8.0 COLUMN_TYPE spellings.
func (c Column) IsBinary() bool {
	return binaryTypes[typeName(c.Type)]
}

// typeName extracts the lower-cased leading type name from a COLUMN_TYPE value,
// e.g. "VARBINARY(16)" -> "varbinary", "bigint(20) unsigned" -> "bigint".
func typeName(columnType string) string {
	t := strings.ToLower(strings.TrimSpace(columnType))
	if i := strings.IndexAny(t, "( "); i >= 0 {
		t = t[:i]
	}
	return t
}
