package cli

import (
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/yamagame/mysql-relation-deleter/internal/collect"
	"github.com/yamagame/mysql-relation-deleter/internal/dbconn"
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
// single-dash flag starting with "p"). It runs before fs.Parse so that the
// parse error, which would echo the flag text, never shows a password.
func passwordFlagGiven(fs *flag.FlagSet, args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || len(a) < 2 || a[0] != '-' {
			return false // the flag package stops parsing here
		}
		doubleDash := strings.HasPrefix(a, "--")
		name := strings.TrimPrefix(a[1:], "-")
		hasValue := false
		if j := strings.IndexByte(name, '='); j >= 0 {
			name, hasValue = name[:j], true
		}
		if name == "p" || name == "password" {
			return true
		}
		f := fs.Lookup(name)
		if f == nil {
			return !doubleDash && strings.HasPrefix(name, "p")
		}
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); !hasValue && !(ok && b.IsBoolFlag()) {
			i++ // skip the flag's separate value
		}
	}
	return false
}

// parsedID is one --id value and its CSV fields.
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

// tupleKey is an injective encoding of a tuple of string values.
func tupleKey(t collect.Tuple) string {
	var b strings.Builder
	for _, v := range t {
		b.WriteString(strconv.Quote(fmt.Sprint(v)))
		b.WriteByte(',')
	}
	return b.String()
}
