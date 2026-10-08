package schema

import "strings"

// QuoteIdent quotes a MySQL identifier with backticks, doubling any backtick
// inside the name. Callers must only pass names that exist in the schema file;
// quoting guarantees that even a tampered name cannot break out of the
// identifier.
func QuoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}
