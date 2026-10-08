package cli

import (
	"fmt"
	"io"
)

// progName prefixes every message written to stderr.
const progName = "relation-deleter"

// usageText is printed by --help.
const usageText = `Usage:
  relation-deleter --schema PATH --table NAME --id VALUE [--id VALUE ...] [options]

Deletes the rows of TABLE whose primary key matches each --id, together with
every row that depends on them through foreign keys in the schema file and
through the manual relations in --relations. Without --execute it only prints
the deletion plan (dry-run) and changes nothing.

Input:
  --schema PATH          schema file written by scripts/dump-schema.sh (required)
  --relations PATH       manual relation definitions (YAML, version 1) for
                         references that have no foreign key
  --table NAME           table of the rows to delete (required)
  --id VALUE             primary key value of a row to delete (required,
                         repeatable). For a composite primary key give the
                         values in primary key order as one CSV record; quote a
                         value that contains a comma: --id '10,"a,b"'.
                         For a binary primary key column (BINARY, VARBINARY,
                         BLOB, BIT) a value starting with 0x or 0X is read as
                         hex: --id 0x0a0b. BINARY(n) values are stored
                         right-padded with 0x00, so give all n bytes. Other
                         values, and 0x values of other columns, are used as
                         text. Duplicate ids are ignored.

Execution:
  --execute              delete the rows (default: dry-run, print the plan only)
  --yes                  skip the confirmation prompt of --execute
  --max-records N        stop without deleting when more than N rows would be
                         deleted (default 0: no limit)
  --chunk-size N         values per IN list in SELECT and DELETE (default 500)

Connection:
  -h, --host HOST        server host (default 127.0.0.1)
  -P, --port PORT        server port (default 3306)
  -u, --user USER        user name
  -D, --database NAME    database (default: the database in the schema file)
  --socket PATH          unix socket; Host and Port are then ignored
  --defaults-file PATH   option file whose [client] section gives user,
                         password, host, port and socket

  --help                 print this help and exit

Credentials:
  There is no password flag. Set the password in the MYSQL_PWD environment
  variable, or put it in the [client] section of the --defaults-file option
  file (keep the file readable only by you: chmod 600). MYSQL_PWD wins when
  both are given. Command-line flags win over the option file.

Notes and limitations:
  - The default is a dry-run. --execute asks for confirmation on a terminal;
    without a terminal it requires --yes.
  - The schema file must match the live database. Dump it again after a
    schema change.
  - A manual relation on columns without an index causes a full table scan
    for every lookup. Measure with a dry-run first.
  - FLOAT and JSON columns used as keys may not match the values read back,
    so rows reached through them can be missed.
  - Rows inserted concurrently through relations without a foreign key are
    not detected.
  - Tables in other databases that reference these tables are not covered.

Exit codes:
  0  success (plan printed, or rows deleted)
  1  runtime error (connection, SQL error, failed delete, count mismatch)
  2  invalid input (flags, schema file, relations file, table or id
     mismatch, no matching rows)
  3  aborted (confirmation declined, --max-records exceeded, or no terminal
     and no --yes)
`

func printUsage(w io.Writer) {
	fmt.Fprint(w, usageText)
}

// printUsageHint points to --help after a flag error.
func printUsageHint(w io.Writer) {
	fmt.Fprintf(w, "%s: run '%s --help' for usage\n", progName, progName)
}
