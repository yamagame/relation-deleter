#!/usr/bin/env bash
#
# dump-schema.sh - dump the table / foreign key structure of a MySQL database
# as a deterministic schema file (format_version 1) for relation-deleter.
#
# Supports MySQL 5.7.8+ and 8.0. Runs on bash 3.2 (macOS) and later.
# See usage() for options, password handling and exit codes.

set -euo pipefail

readonly PROG=${0##*/}
readonly MIN_VERSION="5.7.8"

# Exit codes.
readonly EXIT_FAILURE=1 # connection or query failure, or failure to write
readonly EXIT_USAGE=2   # bad arguments or unsupported server version

usage() {
	cat <<EOF
Usage: $PROG -h HOST -P PORT -u USER -D DATABASE -o OUTPUT [--defaults-file FILE]
       $PROG --socket SOCKET -u USER -D DATABASE -o OUTPUT [--defaults-file FILE]

Dump the tables (views excluded), columns, primary keys and foreign keys of
DATABASE to the schema file OUTPUT (JSON, format_version 1). Rows are never
read. OUTPUT is replaced only when the dump succeeds.

Options:
  -h, --host HOST          server host (required unless --socket is given)
  -P, --port PORT          server TCP port (required unless --socket is given)
      --socket SOCKET      connect through a Unix socket instead of host/port
  -u, --user USER          user name (required)
  -D, --database DATABASE  database to dump (required)
  -o, --output OUTPUT      schema file to write (required)
      --defaults-file FILE MySQL option file, passed to mysql as
                           --defaults-extra-file (e.g. [client] password=...)
      --help               show this help and exit

Password:
  There is no option to pass a password on the command line. Use one of:
    - the MYSQL_PWD environment variable. The script moves it to a temporary
      option file (mode 600, removed on exit) and removes it from the
      environment, so it never appears in process arguments or output.
    - --defaults-file FILE with a [client] section containing password=...
  If both are given, MYSQL_PWD wins over a password in --defaults-file (the
  same order as relation-deleter); other settings in the file still apply.

Requirements:
  MySQL $MIN_VERSION or later (5.7 or 8.0) and the mysql command-line client.
  MariaDB is not supported. If jq is installed, the output is checked to be
  valid JSON before it replaces OUTPUT.

Exit codes:
  0  success; "tables=N foreign_keys=M" is printed to stderr
  1  connection, query or write failure (an existing OUTPUT is left untouched)
  2  invalid arguments, or the server is older than $MIN_VERSION
EOF
}

err() {
	printf '%s: %s\n' "$PROG" "$*" >&2
}

die() {
	local code=$1
	shift
	err "$@"
	exit "$code"
}

usage_error() {
	err "$@"
	printf "Try '%s --help' for more information.\n" "$PROG" >&2
	exit "$EXIT_USAGE"
}

# ---------------------------------------------------------------------------
# Temporary files and cleanup
# ---------------------------------------------------------------------------

OPT_FILE=""
TMP_OUT=""

# Invoked only through the EXIT trap below.
# shellcheck disable=SC2329
cleanup() {
	if [ -n "$OPT_FILE" ]; then
		rm -f -- "$OPT_FILE"
	fi
	if [ -n "$TMP_OUT" ]; then
		rm -f -- "$TMP_OUT"
	fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Remember the caller's umask for the final output file, then make every file
# this script creates private (the temporary option file holds a password).
ORIG_UMASK=$(umask)
umask 077

# Take the password out of the environment right away so no child process
# (mysql, mktemp, ...) inherits it. Bash 3.2 has no ${var@Q}; test with +x.
PASSWORD=""
HAVE_PASSWORD=0
if [ -n "${MYSQL_PWD+x}" ]; then
	PASSWORD=$MYSQL_PWD
	HAVE_PASSWORD=1
	unset MYSQL_PWD
fi

# ---------------------------------------------------------------------------
# Arguments
# ---------------------------------------------------------------------------

HOST=""
PORT=""
SOCKET=""
DB_USER=""
DATABASE=""
OUTPUT=""
DEFAULTS_FILE=""

need_value() {
	# $1: option name, $2: number of remaining arguments including the option
	if [ "$2" -lt 2 ]; then
		usage_error "option $1 requires a value"
	fi
}

while [ $# -gt 0 ]; do
	case $1 in
	-h | --host)
		need_value "$1" $#
		HOST=$2
		shift 2
		;;
	-P | --port)
		need_value "$1" $#
		PORT=$2
		shift 2
		;;
	--socket)
		need_value "$1" $#
		SOCKET=$2
		shift 2
		;;
	-u | --user)
		need_value "$1" $#
		DB_USER=$2
		shift 2
		;;
	-D | --database)
		need_value "$1" $#
		DATABASE=$2
		shift 2
		;;
	-o | --output)
		need_value "$1" $#
		OUTPUT=$2
		shift 2
		;;
	--defaults-file)
		need_value "$1" $#
		DEFAULTS_FILE=$2
		shift 2
		;;
	--defaults-file=*)
		DEFAULTS_FILE=${1#*=}
		shift
		;;
	--help)
		usage
		exit 0
		;;
	-p* | --password*)
		usage_error "passwords cannot be passed as arguments; use MYSQL_PWD or --defaults-file"
		;;
	*)
		usage_error "unknown argument: $1"
		;;
	esac
done

missing=""
if [ -z "$SOCKET" ]; then
	[ -n "$HOST" ] || missing="$missing -h"
	[ -n "$PORT" ] || missing="$missing -P"
fi
[ -n "$DB_USER" ] || missing="$missing -u"
[ -n "$DATABASE" ] || missing="$missing -D"
[ -n "$OUTPUT" ] || missing="$missing -o"
if [ -n "$missing" ]; then
	usage_error "missing required argument(s):$missing"
fi

case $PORT in
*[!0-9]*) usage_error "invalid port: $PORT" ;;
esac

if [ -n "$DEFAULTS_FILE" ] && [ ! -r "$DEFAULTS_FILE" ]; then
	usage_error "cannot read defaults file: $DEFAULTS_FILE"
fi
case $DEFAULTS_FILE in
*$'\n'* | *$'\r'*)
	usage_error "defaults file path must not contain line breaks"
	;;
esac

OUTPUT_DIR=$(dirname -- "$OUTPUT")
OUTPUT_BASE=$(basename -- "$OUTPUT")
if [ ! -d "$OUTPUT_DIR" ]; then
	usage_error "output directory does not exist: $OUTPUT_DIR"
fi
if [ -d "$OUTPUT" ]; then
	usage_error "output is a directory: $OUTPUT"
fi

# ---------------------------------------------------------------------------
# Option file for the password
# ---------------------------------------------------------------------------

MYSQL_OPTION_FILE=""
if [ "$HAVE_PASSWORD" -eq 1 ]; then
	case $PASSWORD in
	*$'\n'* | *$'\r'*)
		usage_error "MYSQL_PWD must not contain line breaks"
		;;
	esac
	OPT_FILE=$(mktemp "${TMPDIR:-/tmp}/dump-schema.XXXXXX") ||
		die "$EXIT_FAILURE" "cannot create a temporary option file"
	chmod 600 "$OPT_FILE"
	# Double-quote the value and escape backslash and double quote. printf is
	# a shell builtin, so the password is never part of a process's argv.
	escaped=${PASSWORD//\\/\\\\}
	escaped=${escaped//\"/\\\"}
	{
		# mysql accepts only one --defaults-extra-file, so the user's file is
		# included first and the password after it: later values win, which
		# gives MYSQL_PWD priority over the file (same order as relation-deleter).
		if [ -n "$DEFAULTS_FILE" ]; then
			printf '!include %s\n' "$DEFAULTS_FILE"
		fi
		printf '[client]\npassword="%s"\n' "$escaped"
	} >"$OPT_FILE"
	escaped=""
	MYSQL_OPTION_FILE=$OPT_FILE
elif [ -n "$DEFAULTS_FILE" ]; then
	MYSQL_OPTION_FILE=$DEFAULTS_FILE
fi
PASSWORD=""

# ---------------------------------------------------------------------------
# mysql invocation
# ---------------------------------------------------------------------------

# Arguments for every mysql call. --defaults-extra-file must come first.
MYSQL_ARGS=()
if [ -n "$MYSQL_OPTION_FILE" ]; then
	MYSQL_ARGS+=("--defaults-extra-file=$MYSQL_OPTION_FILE")
fi
MYSQL_ARGS+=(-N -B -r --default-character-set=utf8mb4 --user="$DB_USER")
if [ -n "$SOCKET" ]; then
	MYSQL_ARGS+=(--protocol=SOCKET --socket="$SOCKET")
else
	MYSQL_ARGS+=(--protocol=TCP --host="$HOST" --port="$PORT")
fi

# run_sql SQL: run SQL in one session, print the raw result rows on stdout.
# mysql's own error messages go to stderr unchanged (they contain no password).
run_sql() {
	mysql "${MYSQL_ARGS[@]}" -e "$1"
}

# sql_string VALUE: VALUE as a utf8mb4 hex string literal. Hex literals need
# no escaping at all, so any database name is safe regardless of sql_mode.
sql_string() {
	local hex
	hex=$(printf '%s' "$1" | od -An -v -tx1 | tr -d ' \n')
	printf "_utf8mb4 X'%s'" "$hex"
}

# ---------------------------------------------------------------------------
# 1. Server version
# ---------------------------------------------------------------------------

if ! VERSION=$(run_sql "SELECT VERSION()"); then
	die "$EXIT_FAILURE" "cannot connect to the server or query its version"
fi

case $VERSION in
*[Mm]ari[Aa][Dd][Bb]*)
	err "warning: MariaDB ($VERSION) is not supported; the dump may fail or be incomplete"
	;;
esac

if [[ $VERSION =~ ^([0-9]+)\.([0-9]+)\.([0-9]+) ]]; then
	v_major=${BASH_REMATCH[1]}
	v_minor=${BASH_REMATCH[2]}
	v_patch=${BASH_REMATCH[3]}
else
	die "$EXIT_FAILURE" "cannot parse server version: $VERSION"
fi
# 10# forces base 10 so that components such as "08" are not read as octal.
v_number=$((10#$v_major * 1000000 + 10#$v_minor * 1000 + 10#$v_patch))
if [ "$v_number" -lt 5007008 ]; then
	die "$EXIT_USAGE" "MySQL $MIN_VERSION or later is required (server version: $VERSION)"
fi

# ---------------------------------------------------------------------------
# 2. Schema queries (one session)
# ---------------------------------------------------------------------------

DB_LIT=$(sql_string "$DATABASE")

# Each result row is "<tag>\t<json>". JSON_OBJECT escapes tabs and line breaks
# inside values, so a row is always exactly one line.
#   H: JSON-quoted database name and server version, and 1 if the database
#      exists (0 otherwise)
#   T: one table, ordered by name (byte order)
#   F: one foreign key, ordered by (table, name) (byte order)
# Nested arrays are built with GROUP_CONCAT(... ORDER BY ...) because
# JSON_ARRAYAGG cannot be ordered, then CAST to JSON so they are embedded as
# arrays, not strings. Booleans are CAST('true'/'false' AS JSON).
read -r -d '' SCHEMA_SQL <<EOF || true
SET SESSION group_concat_max_len = 67108864;
SELECT 'H', JSON_QUOTE(CONVERT($DB_LIT USING utf8mb4)), JSON_QUOTE(VERSION()),
  (SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = $DB_LIT);
SELECT 'T', JSON_OBJECT(
  'name', t.TABLE_NAME,
  'columns', CAST((
    SELECT CONCAT('[', IFNULL(GROUP_CONCAT(
      JSON_OBJECT(
        'name', c.COLUMN_NAME,
        'type', c.COLUMN_TYPE,
        'nullable', IF(c.IS_NULLABLE = 'YES', CAST('true' AS JSON), CAST('false' AS JSON)))
      ORDER BY c.ORDINAL_POSITION SEPARATOR ','), ''), ']')
    FROM information_schema.COLUMNS c
    WHERE c.TABLE_SCHEMA = t.TABLE_SCHEMA AND c.TABLE_NAME = t.TABLE_NAME
  ) AS JSON),
  'primary_key', CAST((
    SELECT CONCAT('[', IFNULL(GROUP_CONCAT(
      JSON_QUOTE(k.COLUMN_NAME) ORDER BY k.ORDINAL_POSITION SEPARATOR ','), ''), ']')
    FROM information_schema.KEY_COLUMN_USAGE k
    WHERE k.TABLE_SCHEMA = t.TABLE_SCHEMA AND k.TABLE_NAME = t.TABLE_NAME
      AND k.CONSTRAINT_NAME = 'PRIMARY'
  ) AS JSON))
FROM information_schema.TABLES t
WHERE t.TABLE_SCHEMA = $DB_LIT AND t.TABLE_TYPE = 'BASE TABLE'
ORDER BY CAST(t.TABLE_NAME AS BINARY);
SELECT 'F', JSON_OBJECT(
  'name', k.CONSTRAINT_NAME,
  'table', k.TABLE_NAME,
  'columns', CAST(CONCAT('[', GROUP_CONCAT(
    JSON_QUOTE(k.COLUMN_NAME) ORDER BY k.ORDINAL_POSITION SEPARATOR ','), ']') AS JSON),
  'referenced_table', k.REFERENCED_TABLE_NAME,
  'referenced_columns', CAST(CONCAT('[', GROUP_CONCAT(
    JSON_QUOTE(k.REFERENCED_COLUMN_NAME) ORDER BY k.ORDINAL_POSITION SEPARATOR ','), ']') AS JSON),
  'delete_rule', rc.DELETE_RULE)
FROM information_schema.KEY_COLUMN_USAGE k
JOIN information_schema.REFERENTIAL_CONSTRAINTS rc
  ON rc.CONSTRAINT_SCHEMA = k.CONSTRAINT_SCHEMA
 AND rc.TABLE_NAME = k.TABLE_NAME
 AND rc.CONSTRAINT_NAME = k.CONSTRAINT_NAME
WHERE k.TABLE_SCHEMA = $DB_LIT
  AND k.REFERENCED_TABLE_SCHEMA = $DB_LIT
GROUP BY k.TABLE_NAME, k.CONSTRAINT_NAME, k.REFERENCED_TABLE_NAME, rc.DELETE_RULE
ORDER BY CAST(k.TABLE_NAME AS BINARY), CAST(k.CONSTRAINT_NAME AS BINARY);
EOF

if ! ROWS=$(run_sql "$SCHEMA_SQL"); then
	die "$EXIT_FAILURE" "failed to read the schema of database '$DATABASE'"
fi

# ---------------------------------------------------------------------------
# 3. Assemble the schema file
# ---------------------------------------------------------------------------

DB_JSON=""
VERSION_JSON=""
DB_EXISTS=""
TABLES_JSON=""
FKS_JSON=""
n_tables=0
n_fks=0
TAB=$'\t'

while IFS= read -r line; do
	tag=${line%%"$TAB"*}
	rest=${line#*"$TAB"}
	case $tag in
	H)
		DB_JSON=${rest%%"$TAB"*}
		rest=${rest#*"$TAB"}
		VERSION_JSON=${rest%%"$TAB"*}
		DB_EXISTS=${rest#*"$TAB"}
		;;
	T)
		if [ "$n_tables" -gt 0 ]; then
			TABLES_JSON="$TABLES_JSON,"$'\n'
		fi
		TABLES_JSON="$TABLES_JSON    $rest"
		n_tables=$((n_tables + 1))
		;;
	F)
		if [ "$n_fks" -gt 0 ]; then
			FKS_JSON="$FKS_JSON,"$'\n'
		fi
		FKS_JSON="$FKS_JSON    $rest"
		n_fks=$((n_fks + 1))
		;;
	*)
		die "$EXIT_FAILURE" "unexpected query output line: $line"
		;;
	esac
done <<EOF
$ROWS
EOF

if [ -z "$DB_JSON" ] || [ -z "$VERSION_JSON" ]; then
	die "$EXIT_FAILURE" "unexpected query output: missing header row"
fi
if [ "$DB_EXISTS" != "1" ]; then
	die "$EXIT_FAILURE" "database not found: $DATABASE"
fi

# json_array NAME BODY: a top-level array member, one element per line.
json_array() {
	if [ -z "$2" ]; then
		printf '  "%s": []' "$1"
	else
		printf '  "%s": [\n%s\n  ]' "$1" "$2"
	fi
}

TMP_OUT=$(mktemp "$OUTPUT_DIR/.$OUTPUT_BASE.XXXXXX") ||
	die "$EXIT_FAILURE" "cannot create a temporary file in $OUTPUT_DIR"

{
	printf '{\n'
	printf '  "format_version": 1,\n'
	printf '  "database": %s,\n' "$DB_JSON"
	printf '  "server_version": %s,\n' "$VERSION_JSON"
	json_array tables "$TABLES_JSON"
	printf ',\n'
	json_array foreign_keys "$FKS_JSON"
	printf '\n}\n'
} >"$TMP_OUT" || die "$EXIT_FAILURE" "cannot write $TMP_OUT"

if [ ! -s "$TMP_OUT" ]; then
	die "$EXIT_FAILURE" "generated schema file is empty"
fi
if command -v jq >/dev/null 2>&1; then
	if ! jq -e . "$TMP_OUT" >/dev/null; then
		die "$EXIT_FAILURE" "generated schema file is not valid JSON"
	fi
fi

# Give the output the permissions the caller's umask would have produced.
chmod "$(printf '%o' $((0666 & ~0$ORIG_UMASK)))" "$TMP_OUT" ||
	die "$EXIT_FAILURE" "cannot set permissions on $TMP_OUT"
mv -f -- "$TMP_OUT" "$OUTPUT" || die "$EXIT_FAILURE" "cannot replace $OUTPUT"
TMP_OUT=""

printf 'tables=%d foreign_keys=%d\n' "$n_tables" "$n_fks" >&2
exit 0
