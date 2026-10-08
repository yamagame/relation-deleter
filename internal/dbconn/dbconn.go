// Package dbconn resolves the MySQL connection settings and opens the
// database connection.
//
// Passwords are never taken from command-line flags: they come from the
// MYSQL_PWD environment variable or the [client] section of an option file
// (--defaults-file). A resolved Config keeps the password in an unexported
// field, and its String, Redacted and error messages never contain it (9.1,
// 9.2).
package dbconn

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Default connection values used when neither a flag nor the option file
// gives one.
const (
	DefaultHost = "127.0.0.1"
	DefaultPort = 3306
)

// PasswordEnv is the environment variable that carries the password.
const PasswordEnv = "MYSQL_PWD"

// connectTimeout bounds the TCP/unix dial of each connection.
const connectTimeout = 10 * time.Second

// collation is the connection collation sent in the handshake. It exists on
// both MySQL 5.7 and 8.0.
const collation = "utf8mb4_unicode_ci"

// Flags holds the connection values given on the command line. A nil pointer
// means the flag was not given. There is deliberately no password field.
type Flags struct {
	Host     *string // -h/--host
	Port     *int    // -P/--port
	User     *string // -u/--user
	Database *string // -D/--database
	Socket   *string // --socket

	DefaultsFile string // --defaults-file; empty means no option file
}

// Config is a resolved connection setting.
//
// When Socket is set the connection uses that unix socket, and Host and Port
// are ignored. Database may be empty.
type Config struct {
	Host, User, Database, Socket string
	Port                         int

	// Warnings are non-fatal problems found while resolving, such as an
	// option file readable by other users. The caller prints them.
	Warnings []string

	password string
}

// Resolve merges the connection settings. For each value the priority is:
// flags > MYSQL_PWD (password only) > option file > defaults.
//
// A user is required. A socket from the option file is ignored when --host or
// --port is given on the command line.
func Resolve(flags Flags, env func(string) string) (Config, error) {
	var file optionValues
	var c Config
	if flags.DefaultsFile != "" {
		v, warn, err := readOptionFile(flags.DefaultsFile)
		if err != nil {
			return Config{}, err
		}
		file = v
		if warn != "" {
			c.Warnings = append(c.Warnings, warn)
		}
	}

	c.Host = pick(flags.Host, file.host, DefaultHost)
	c.User = pick(flags.User, file.user, "")
	c.Database = pick(flags.Database, nil, "")
	switch {
	case flags.Socket != nil:
		c.Socket = *flags.Socket
	case flags.Host == nil && flags.Port == nil && file.socket != nil:
		c.Socket = *file.socket
	}

	c.Port = DefaultPort
	switch {
	case flags.Port != nil:
		c.Port = *flags.Port
	case file.port != nil:
		c.Port = *file.port
	}
	if c.Port < 1 || c.Port > 65535 {
		return Config{}, fmt.Errorf("invalid port %d: must be between 1 and 65535", c.Port)
	}

	if pw := env(PasswordEnv); pw != "" {
		c.password = pw
	} else if file.password != nil {
		c.password = *file.password
	}

	if c.User == "" {
		if flags.DefaultsFile != "" {
			return Config{}, fmt.Errorf("no user given: use -u/--user or set user in the [client] section of %s", flags.DefaultsFile)
		}
		return Config{}, errors.New("no user given: use -u/--user or set user in the [client] section of --defaults-file")
	}
	return c, nil
}

func pick(flag, file *string, def string) string {
	switch {
	case flag != nil:
		return *flag
	case file != nil:
		return *file
	default:
		return def
	}
}

// driverConfig builds the driver configuration, including the password.
func (c Config) driverConfig() *mysql.Config {
	cfg := mysql.NewConfig()
	if c.Socket != "" {
		cfg.Net = "unix"
		cfg.Addr = c.Socket
	} else {
		cfg.Net = "tcp"
		cfg.Addr = net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	}
	cfg.User = c.User
	cfg.Passwd = c.password
	cfg.DBName = c.Database
	cfg.ParseTime = false
	cfg.InterpolateParams = false
	cfg.Collation = collation
	cfg.Timeout = connectTimeout
	return cfg
}

// DSN returns the data source name. It contains the password: never print it
// or put it in an error.
func (c Config) DSN() string { return c.driverConfig().FormatDSN() }

// Redacted returns the connection target without the password, as
// "user@host:port/db" or "user@unix(socket)/db". Use it in messages.
func (c Config) Redacted() string {
	if c.Socket != "" {
		return fmt.Sprintf("%s@unix(%s)/%s", c.User, c.Socket, c.Database)
	}
	return fmt.Sprintf("%s@%s/%s", c.User, net.JoinHostPort(c.Host, strconv.Itoa(c.Port)), c.Database)
}

// String returns Redacted, so that printing a Config never shows the password.
func (c Config) String() string { return c.Redacted() }

// GoString keeps %#v from printing the unexported password field.
func (c Config) GoString() string { return "dbconn.Config(" + c.Redacted() + ")" }

// Open opens the database and pings it. A failure is returned as
// "connect to <Redacted>: <cause>"; the DSN and password are never included.
func Open(ctx context.Context, c Config) (*sql.DB, error) {
	connector, err := mysql.NewConnector(c.driverConfig())
	if err != nil {
		return nil, c.connectError(err)
	}
	db := sql.OpenDB(connector)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, c.connectError(err)
	}
	return db, nil
}

// connectError adds the redacted target to err. The driver does not put the
// password in its errors, but if the message ever contains it, the password
// is masked and the original error is dropped so it cannot be unwrapped.
func (c Config) connectError(err error) error {
	if c.password != "" && strings.Contains(err.Error(), c.password) {
		return fmt.Errorf("connect to %s: %s", c.Redacted(), strings.ReplaceAll(err.Error(), c.password, "****"))
	}
	return fmt.Errorf("connect to %s: %w", c.Redacted(), err)
}

// optionValues holds the [client] values of an option file. nil means not set.
type optionValues struct {
	user, password, host, socket *string
	port                         *int
}

// readOptionFile reads the [client] section of an option file. It also
// returns a warning when the file is accessible by the group or others.
func readOptionFile(path string) (optionValues, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return optionValues{}, "", fmt.Errorf("read option file: %w", err) // *PathError includes the path
	}
	var warn string
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		warn = fmt.Sprintf("option file %s has permissions %04o; it should be 0600 or stricter because it may contain a password",
			path, fi.Mode().Perm())
	}
	v, err := parseOptionFile(data)
	if err != nil {
		return optionValues{}, "", fmt.Errorf("option file %s: %w", path, err)
	}
	return v, warn, nil
}

// parseOptionFile parses MySQL option file syntax and keeps the known keys of
// every [client] section (a later value wins). Errors never contain values.
func parseOptionFile(data []byte) (optionValues, error) {
	var v optionValues
	inClient := false
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' || line[0] == '!' {
			continue // blank, comment, or !include directive
		}
		if line[0] == '[' {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				return optionValues{}, fmt.Errorf("line %d: unterminated section header", lineNo)
			}
			inClient = strings.EqualFold(strings.TrimSpace(line[1:end]), "client")
			continue
		}
		if !inClient {
			continue
		}
		key, raw, hasValue := strings.Cut(line, "=")
		key = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "_", "-"))
		if !hasValue {
			continue // boolean option such as no-beep
		}
		switch key {
		case "user", "password", "host", "socket", "port":
		default:
			continue // unknown keys are ignored
		}
		value, err := unquote(strings.TrimSpace(raw))
		if err != nil {
			return optionValues{}, fmt.Errorf("line %d: %s: %w", lineNo, key, err)
		}
		switch key {
		case "user":
			v.user = &value
		case "password":
			v.password = &value
		case "host":
			v.host = &value
		case "socket":
			v.socket = &value
		case "port":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 65535 {
				return optionValues{}, fmt.Errorf("line %d: port must be a number between 1 and 65535", lineNo)
			}
			v.port = &n
		}
	}
	if err := sc.Err(); err != nil {
		return optionValues{}, err
	}
	return v, nil
}

// unquote decodes a value. Single quotes are literal. Double quotes support
// the backslash escapes \" \\ \' \n \t \r \b \s and \0. Unquoted values are
// used as they are. Errors never include the value.
func unquote(s string) (string, error) {
	if s == "" || (s[0] != '"' && s[0] != '\'') {
		return s, nil
	}
	q := s[0]
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == q:
			if rest := strings.TrimSpace(s[i+1:]); rest != "" && rest[0] != '#' {
				return "", errors.New("unexpected text after the closing quote")
			}
			return b.String(), nil
		case ch == '\\' && q == '"' && i+1 < len(s):
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b':
				b.WriteByte('\b')
			case 's':
				b.WriteByte(' ')
			case '0':
				b.WriteByte(0)
			default: // \" \\ \' and anything else: the character itself
				b.WriteByte(s[i])
			}
		default:
			b.WriteByte(ch)
		}
	}
	return "", errors.New("missing closing quote")
}
