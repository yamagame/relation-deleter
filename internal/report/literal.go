package report

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/yamagame/relation-deleter/internal/collect"
)

var (
	// ErrLiteralType is returned for a value that is not string, []byte or nil.
	ErrLiteralType = errors.New("unsupported value type for a literal")
	// ErrArgCount is returned when a statement's placeholders and arguments differ in number.
	ErrArgCount = errors.New("placeholder count does not match the argument count")
)

// literal renders v as a MySQL literal for display only (5.3); execution
// always uses placeholders. A string becomes '...' with \, ', NUL, \n, \r and
// \x1a escaped; a []byte becomes 0x followed by lowercase hex, or the
// empty hex literal (X and two single quotes) when it has no bytes; nil
// becomes NULL. Any other type is an error.
func literal(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "NULL", nil
	case string:
		var b strings.Builder
		b.Grow(len(x) + 2)
		b.WriteByte('\'')
		for i := 0; i < len(x); i++ {
			switch c := x[i]; c {
			case '\\':
				b.WriteString(`\\`)
			case '\'':
				b.WriteString(`\'`)
			case 0:
				b.WriteString(`\0`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case 0x1a:
				b.WriteString(`\Z`)
			default:
				b.WriteByte(c)
			}
		}
		b.WriteByte('\'')
		return b.String(), nil
	case []byte:
		if len(x) == 0 {
			return "X''", nil
		}
		return "0x" + hex.EncodeToString(x), nil
	default:
		return "", fmt.Errorf("%w: %T", ErrLiteralType, v)
	}
}

// tupleLiteral renders a one-value tuple as the bare literal and a longer one
// as (v1,v2,...).
func tupleLiteral(t collect.Tuple) (string, error) {
	parts := make([]string, len(t))
	for i, v := range t {
		s, err := literal(v)
		if err != nil {
			return "", err
		}
		parts[i] = s
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return "(" + strings.Join(parts, ",") + ")", nil
}

// expandSQL replaces each ? placeholder of sql, in order, with literal(args[i])
// for display. A ? inside a backtick-quoted identifier (where a backtick is
// escaped by doubling it) is not a placeholder. The number of placeholders
// must equal len(args).
func expandSQL(sql string, args []any) (string, error) {
	var b strings.Builder
	b.Grow(len(sql) + 8*len(args))
	n := 0
	inIdent := false
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch {
		case c == '`':
			// A doubled backtick inside an identifier toggles twice and
			// therefore stays inside it.
			inIdent = !inIdent
			b.WriteByte(c)
		case c == '?' && !inIdent:
			if n >= len(args) {
				return "", fmt.Errorf("%w: more than %d placeholders in %q", ErrArgCount, len(args), sql)
			}
			s, err := literal(args[n])
			if err != nil {
				return "", fmt.Errorf("argument %d: %w", n, err)
			}
			b.WriteString(s)
			n++
		default:
			b.WriteByte(c)
		}
	}
	if n != len(args) {
		return "", fmt.Errorf("%w: %d placeholders, %d arguments in %q", ErrArgCount, n, len(args), sql)
	}
	return b.String(), nil
}
