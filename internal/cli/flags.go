package cli

import (
	"encoding/binary"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/yamagame/mysql-relation-deleter/internal/collect"
	"github.com/yamagame/mysql-relation-deleter/internal/dbconn"
	"github.com/yamagame/mysql-relation-deleter/internal/schema"
)

// defaultChunkSize is the default of --chunk-size.
const defaultChunkSize = 500

// rawFlags holds the command-line values as parsed, before validation.
type rawFlags struct {
	schemaPath    string
	relationsPath string
	table         string
	ids           idList
	execute       bool
	yes           bool
	maxRecords    int
	chunkSize     int

	host, user, database, socket, defaultsFile string
	port                                       int
}

// idList collects every --id value in command-line order.
type idList []string

func (l *idList) String() string {
	if l == nil {
		return ""
	}
	return strings.Join(*l, " ")
}

func (l *idList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// newFlagSet defines the flags of the design's flag table on r. Short and
// long names share one variable. The flag package accepts both -x and --x.
// There is deliberately no password flag (9.1). "help" is not defined, so
// -help/--help makes Parse return flag.ErrHelp; -h is the host.
//
// The flag set prints nothing: Run reports parse errors itself.
func newFlagSet(r *rawFlags) *flag.FlagSet {
	fs := flag.NewFlagSet(progName, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	fs.StringVar(&r.schemaPath, "schema", "", "")
	fs.StringVar(&r.relationsPath, "relations", "", "")
	fs.StringVar(&r.table, "table", "", "")
	fs.Var(&r.ids, "id", "")
	fs.BoolVar(&r.execute, "execute", false, "")
	fs.BoolVar(&r.yes, "yes", false, "")
	fs.IntVar(&r.maxRecords, "max-records", 0, "")
	fs.IntVar(&r.chunkSize, "chunk-size", defaultChunkSize, "")

	for _, n := range []string{"h", "host"} {
		fs.StringVar(&r.host, n, "", "")
	}
	for _, n := range []string{"P", "port"} {
		fs.IntVar(&r.port, n, 0, "")
	}
	for _, n := range []string{"u", "user"} {
		fs.StringVar(&r.user, n, "", "")
	}
	for _, n := range []string{"D", "database"} {
		fs.StringVar(&r.database, n, "", "")
	}
	fs.StringVar(&r.socket, "socket", "", "")
	fs.StringVar(&r.defaultsFile, "defaults-file", "", "")
	return fs
}

// connFlags returns the connection flags, setting a pointer only for a flag
// that was given on the command line (dbconn.Flags treats nil as unset).
func connFlags(fs *flag.FlagSet, r *rawFlags) dbconn.Flags {
	var c dbconn.Flags
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "h", "host":
			c.Host = &r.host
		case "P", "port":
			c.Port = &r.port
		case "u", "user":
			c.User = &r.user
		case "D", "database":
			c.Database = &r.database
		case "socket":
			c.Socket = &r.socket
		case "defaults-file":
			c.DefaultsFile = r.defaultsFile
		}
	})
	return c
}

// errPasswordFlag is reported for -p/--password. Its message never contains
// the value that was given.
var errPasswordFlag = errors.New("there is no password flag (-p/--password); set the MYSQL_PWD environment variable or use --defaults-file with a [client] section")

// passwordFlagGiven reports whether args contain a password flag in the
// part the flag package would parse: -p, --password, either with "=value",
// or a MySQL-client style attached value such as -psecret (any undefined
// single-dash flag starting with "p"). It runs before fs.Parse so that a
// parse error, which echoes the offending text, never shows a password.
//
// A password-like token is also rejected as the value of a flag whose value
// the flag package converts (int and bool flags), as in "-P -psecret" or
// "--execute=-psecret": a conversion error would echo it. String values
// (--table, --id, ...) are accepted as is and never cause a parse error, so
// they are left alone.
func passwordFlagGiven(fs *flag.FlagSet, args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || len(a) < 2 || a[0] != '-' {
			return false // the flag package stops parsing here
		}
		name := strings.TrimPrefix(a[1:], "-")
		value, hasValue := "", false
		if j := strings.IndexByte(name, '='); j >= 0 {
			name, value, hasValue = name[:j], name[j+1:], true
		}
		if isPasswordToken(fs, a) {
			return true
		}
		f := fs.Lookup(name)
		if f == nil {
			return false // unknown flag: Parse reports its name
		}
		converted := convertsValue(f)
		if !hasValue && !isBoolFlag(f) && i+1 < len(args) {
			i++ // the flag's separate value
			value, hasValue = args[i], true
		}
		if hasValue && converted && isPasswordToken(fs, value) {
			return true
		}
	}
	return false
}

// isPasswordToken reports whether tok is -p, --password (with or without
// "=value"), or an undefined single-dash flag starting with "p" (-psecret).
func isPasswordToken(fs *flag.FlagSet, tok string) bool {
	if len(tok) < 2 || tok[0] != '-' {
		return false
	}
	doubleDash := strings.HasPrefix(tok, "--")
	name := strings.TrimPrefix(tok[1:], "-")
	if j := strings.IndexByte(name, '='); j >= 0 {
		name = name[:j]
	}
	if name == "p" || name == "password" {
		return true
	}
	return !doubleDash && strings.HasPrefix(name, "p") && fs.Lookup(name) == nil
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// convertsValue reports whether Parse converts the flag's value and can
// therefore fail with an error that echoes it (every flag except string
// flags and --id).
func convertsValue(f *flag.Flag) bool {
	g, ok := f.Value.(flag.Getter)
	if !ok {
		return false // idList accepts any text
	}
	_, isString := g.Get().(string)
	return !isString
}

// parsedID is one --id value and its fields: strings after CSV parsing, and
// after checkRoots, []byte for hex values of binary primary key columns.
type parsedID struct {
	raw    string
	values collect.Tuple
}

// parseID parses one --id value as a single CSV record (encoding/csv), so a
// value containing a comma can be quoted: 10,"a,b". Fields are taken as is,
// without trimming spaces.
func parseID(v string) ([]string, error) {
	r := csv.NewReader(strings.NewReader(v))
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	switch len(records) {
	case 0:
		return nil, errors.New("empty value")
	case 1:
		return records[0], nil
	default:
		return nil, errors.New("contains a line break outside quotes; give one record per --id")
	}
}

// dedupeIDs removes ids whose values equal an earlier one, keeping the
// first-seen order.
func dedupeIDs(ids []parsedID) []parsedID {
	seen := make(map[string]bool, len(ids))
	out := make([]parsedID, 0, len(ids))
	for _, id := range ids {
		k := tupleKey(id.values)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, id)
	}
	return out
}

// tupleKey is an injective encoding of a tuple whose values are string or
// []byte: each value is a type tag followed by its length and bytes, so a
// []byte and a string with the same bytes (or the same fmt rendering) never
// collide.
func tupleKey(t collect.Tuple) string {
	var b []byte
	for _, v := range t {
		switch x := v.(type) {
		case []byte:
			b = append(b, 'b')
			b = binary.AppendUvarint(b, uint64(len(x)))
			b = append(b, x...)
		default:
			str := fmt.Sprint(x) // only strings occur; anything else is still tagged apart
			if _, ok := x.(string); ok {
				b = append(b, 's')
			} else {
				b = append(b, '?')
			}
			b = binary.AppendUvarint(b, uint64(len(str)))
			b = append(b, str...)
		}
	}
	return string(b)
}

// rootValue converts one --id field for a primary key column. For a binary
// column a value starting with 0x or 0X is decoded as hex ("0x" alone is an
// empty value); any other value, and every value of a non-binary column, is
// kept as the string given.
func rootValue(col schema.Column, field string) (collect.Value, error) {
	if !col.IsBinary() || len(field) < 2 || field[0] != '0' || (field[1] != 'x' && field[1] != 'X') {
		return field, nil
	}
	digits := field[2:]
	if len(digits)%2 != 0 {
		return nil, fmt.Errorf("invalid hex value %q: odd length", field)
	}
	out, err := hex.DecodeString(digits)
	if err != nil {
		var ib hex.InvalidByteError
		if errors.As(err, &ib) {
			return nil, fmt.Errorf("invalid hex value %q: non-hex character %q", field, rune(ib))
		}
		return nil, fmt.Errorf("invalid hex value %q: %v", field, err)
	}
	return out, nil
}
