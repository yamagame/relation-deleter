// Package relations loads the manual relation definition file (version 1) and
// validates it against a schema. Manual relations supplement foreign keys that
// are not declared as constraints in the database.
//
// It depends only on the schema package and the YAML library.
package relations

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/yamagame/relation-deleter/internal/schema"
)

// SupportedVersion is the only definition file version this package reads.
const SupportedVersion = 1

// Endpoint is one side of a manual relation. Columns are matched by position:
// Child.Columns[i] references Parent.Columns[i].
type Endpoint struct {
	Table   string   `yaml:"table"`
	Columns []string `yaml:"columns"`
}

// ManualRelation is one relation declared in the definition file.
type ManualRelation struct {
	Name   string   `yaml:"name"` // optional; defaults to "manual#<index>" (0-based position in relations)
	Child  Endpoint `yaml:"child"`
	Parent Endpoint `yaml:"parent"`
	Line   int      `yaml:"-"` // line of the relation's sequence item
}

// ValidationError is one problem found in the definition file.
type ValidationError struct {
	Path    string
	Line    int
	Message string // e.g. `child.columns[1] "user_id" not found in table "orders"`
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s:%d: %s", e.Path, e.Line, e.Message)
}

// ValidationErrors lists every problem found in the definition file, ordered
// by line. Its Error method prints one "path:line: message" per line.
type ValidationErrors []ValidationError

func (e ValidationErrors) Error() string {
	lines := make([]string, len(e))
	for i, ve := range e {
		lines[i] = ve.Error()
	}
	return strings.Join(lines, "\n")
}

// file is the top level of the definition file. Pointers distinguish a
// missing key from a zero value.
type file struct {
	Version   *int              `yaml:"version"`
	Relations *[]ManualRelation `yaml:"relations"`
}

// Load reads the definition file at path and validates it against s, which
// must already have been loaded with schema.Load.
//
// An unreadable file or a YAML syntax error is returned as an error wrapping
// the cause. Every other problem (unknown keys, wrong types, missing or
// unsupported version, missing relations key, unknown tables or columns,
// column count mismatch, empty column lists) is collected and returned as
// ValidationErrors, each with its line. An explicit empty list
// ("relations: []") is valid and yields no relations.
//
// On success the relations are returned in file order.
func Load(path string, s *schema.Schema) ([]ManualRelation, error) {
	if s == nil {
		return nil, fmt.Errorf("relations file %s: schema is not loaded", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("relations file %s: cannot read file: %w", path, err)
	}

	// The node tree supplies line numbers; a syntax error stops here.
	var doc yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("relations file %s: invalid YAML: %w", path, err)
	}

	v := &validator{path: path, schema: s}

	// Decode strictly so that unknown keys and wrong types become errors.
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		var te *yaml.TypeError
		if !errors.As(err, &te) {
			return nil, fmt.Errorf("relations file %s: invalid YAML: %w", path, err)
		}
		for _, msg := range te.Errors {
			v.addDecodeError(msg)
		}
	}

	root := documentRoot(&doc)
	v.checkVersion(f.Version, root)
	if f.Relations == nil {
		v.add(nodeLine(root), "relations is required")
	}
	var rels []ManualRelation
	if f.Relations != nil {
		rels = *f.Relations
	}
	items := sequenceItems(mappingValue(root, "relations"))
	for i := range rels {
		item := itemAt(items, i)
		r := &rels[i]
		r.Line = nodeLine(item)
		if r.Name == "" {
			r.Name = "manual#" + strconv.Itoa(i)
		}
		v.checkRelation(r, item)
	}

	if len(v.errs) > 0 {
		sort.SliceStable(v.errs, func(i, j int) bool { return v.errs[i].Line < v.errs[j].Line })
		return nil, v.errs
	}
	if rels == nil {
		rels = []ManualRelation{}
	}
	return rels, nil
}

type validator struct {
	path   string
	schema *schema.Schema
	errs   ValidationErrors
}

func (v *validator) add(line int, format string, args ...any) {
	v.errs = append(v.errs, ValidationError{Path: v.path, Line: line, Message: fmt.Sprintf(format, args...)})
}

var (
	decodeLineRe   = regexp.MustCompile(`(?s)^line (\d+): (.*)$`)
	unknownFieldRe = regexp.MustCompile(`^field (\S+) not found in type \S+$`)
)

// addDecodeError converts one yaml.TypeError entry ("line N: ...") into a
// ValidationError, rewording unknown-field errors.
func (v *validator) addDecodeError(msg string) {
	line := 1
	if m := decodeLineRe.FindStringSubmatch(msg); m != nil {
		line, _ = strconv.Atoi(m[1])
		msg = m[2]
	}
	if m := unknownFieldRe.FindStringSubmatch(msg); m != nil {
		msg = fmt.Sprintf("unknown key %q", m[1])
	}
	v.add(line, "%s", msg)
}

func (v *validator) checkVersion(version *int, root *yaml.Node) {
	switch {
	case version == nil:
		// A version of the wrong type is already reported by the decoder.
		if mappingValue(root, "version") == nil {
			v.add(nodeLine(root), "version is required")
		}
	case *version != SupportedVersion:
		v.add(lineOr(mappingValue(root, "version"), nodeLine(root)), "unsupported version %d (want %d)", *version, SupportedVersion)
	}
}

func (v *validator) checkRelation(r *ManualRelation, item *yaml.Node) {
	childOK := v.checkEndpoint("child", r.Child, mappingValue(item, "child"), r.Line)
	parentOK := v.checkEndpoint("parent", r.Parent, mappingValue(item, "parent"), r.Line)
	if childOK && parentOK && len(r.Child.Columns) != len(r.Parent.Columns) {
		v.add(r.Line, "column count mismatch: child has %d, parent has %d", len(r.Child.Columns), len(r.Parent.Columns))
	}
}

// checkEndpoint validates one side of a relation and reports whether it has
// at least one column (so that the column count comparison is meaningful).
func (v *validator) checkEndpoint(side string, ep Endpoint, node *yaml.Node, itemLine int) bool {
	epLine := lineOr(node, itemLine)
	var table *schema.Table
	switch {
	case ep.Table == "":
		v.add(epLine, "%s.table is required", side)
	default:
		t, ok := v.schema.Table(ep.Table)
		if !ok {
			v.add(lineOr(mappingValue(node, "table"), epLine), "%s.table %q not found in schema", side, ep.Table)
		} else {
			table = t
		}
	}

	colsNode := mappingValue(node, "columns")
	if len(ep.Columns) == 0 {
		v.add(lineOr(colsNode, epLine), "%s.columns must not be empty", side)
		return false
	}
	if table != nil {
		colItems := sequenceItems(colsNode)
		for j, c := range ep.Columns {
			if _, ok := table.Column(c); !ok {
				v.add(lineOr(itemAt(colItems, j), lineOr(colsNode, epLine)),
					"%s.columns[%d] %q not found in table %q", side, j, c, ep.Table)
			}
		}
	}
	return true
}

// documentRoot returns the top-level content node, or nil for an empty file.
func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	return nil
}

// mappingValue returns the value node for key in mapping node m, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// sequenceItems returns the items of sequence node n, or nil.
func sequenceItems(n *yaml.Node) []*yaml.Node {
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	return n.Content
}

func itemAt(items []*yaml.Node, i int) *yaml.Node {
	if i < len(items) {
		return items[i]
	}
	return nil
}

// nodeLine returns n's line, or 1 when n is nil (empty file).
func nodeLine(n *yaml.Node) int { return lineOr(n, 1) }

func lineOr(n *yaml.Node, fallback int) int {
	if n == nil || n.Line == 0 {
		return fallback
	}
	return n.Line
}
