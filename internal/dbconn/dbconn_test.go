package dbconn

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

const testPassword = `S3cr3t-PW!`

func strp(s string) *string { return &s }
func intp(n int) *int       { return &n }

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// writeOptionFile writes content to a new file with mode 0600 and returns its path.
func writeOptionFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "my.cnf")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil { // umask may have narrowed it; make it exact
		t.Fatal(err)
	}
	return path
}

// assertNoPassword fails if s contains the test password.
func assertNoPassword(t *testing.T, what, s string) {
	t.Helper()
	if strings.Contains(s, testPassword) {
		t.Errorf("%s contains the password: %q", what, s)
	}
}

func TestResolveDefaults(t *testing.T) {
	c, err := Resolve(Flags{User: strp("alice")}, envOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != "127.0.0.1" || c.Port != 3306 || c.User != "alice" || c.Database != "" || c.Socket != "" {
		t.Errorf("got %+v", c)
	}
	if c.password != "" {
		t.Errorf("password = %q, want empty", c.password)
	}
	if len(c.Warnings) != 0 {
		t.Errorf("warnings = %v", c.Warnings)
	}
}

func TestResolveRequiresUser(t *testing.T) {
	_, err := Resolve(Flags{}, envOf(map[string]string{"MYSQL_PWD": testPassword}))
	if err == nil {
		t.Fatal("expected an error when no user is given")
	}
	if !strings.Contains(err.Error(), "user") {
		t.Errorf("error should mention the user: %v", err)
	}
	assertNoPassword(t, "error", err.Error())
}

func TestResolvePriority(t *testing.T) {
	file := "[client]\nuser=fileuser\npassword=filepw\nhost=filehost\nport=3310\n"
	tests := []struct {
		name     string
		flags    Flags
		env      map[string]string
		wantHost string
		wantPort int
		wantUser string
		wantDB   string
		wantPW   string
	}{
		{
			name:     "option file beats defaults",
			wantHost: "filehost", wantPort: 3310, wantUser: "fileuser", wantPW: "filepw",
		},
		{
			name:     "flags beat option file",
			flags:    Flags{Host: strp("flaghost"), Port: intp(3320), User: strp("flaguser"), Database: strp("flagdb")},
			wantHost: "flaghost", wantPort: 3320, wantUser: "flaguser", wantDB: "flagdb", wantPW: "filepw",
		},
		{
			name:     "MYSQL_PWD beats option file",
			env:      map[string]string{"MYSQL_PWD": testPassword},
			wantHost: "filehost", wantPort: 3310, wantUser: "fileuser", wantPW: testPassword,
		},
		{
			name:     "partial flags keep other values from option file",
			flags:    Flags{Port: intp(4000)},
			wantHost: "filehost", wantPort: 4000, wantUser: "fileuser", wantPW: "filepw",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags := tt.flags
			flags.DefaultsFile = writeOptionFile(t, file)
			c, err := Resolve(flags, envOf(tt.env))
			if err != nil {
				t.Fatal(err)
			}
			if c.Host != tt.wantHost || c.Port != tt.wantPort || c.User != tt.wantUser || c.Database != tt.wantDB {
				t.Errorf("got host=%q port=%d user=%q db=%q, want %q %d %q %q",
					c.Host, c.Port, c.User, c.Database, tt.wantHost, tt.wantPort, tt.wantUser, tt.wantDB)
			}
			if c.password != tt.wantPW {
				t.Errorf("password = %q, want %q", c.password, tt.wantPW)
			}
		})
	}
}

func TestResolveMySQLPwdWithoutOptionFile(t *testing.T) {
	c, err := Resolve(Flags{User: strp("u")}, envOf(map[string]string{"MYSQL_PWD": testPassword}))
	if err != nil {
		t.Fatal(err)
	}
	if c.password != testPassword {
		t.Errorf("password = %q", c.password)
	}
}

func TestResolveSocket(t *testing.T) {
	t.Run("flag socket", func(t *testing.T) {
		c, err := Resolve(Flags{User: strp("u"), Socket: strp("/tmp/mysql.sock"), Database: strp("app")}, envOf(nil))
		if err != nil {
			t.Fatal(err)
		}
		if c.Socket != "/tmp/mysql.sock" {
			t.Fatalf("socket = %q", c.Socket)
		}
		cfg, err := mysql.ParseDSN(c.DSN())
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Net != "unix" || cfg.Addr != "/tmp/mysql.sock" {
			t.Errorf("net=%q addr=%q", cfg.Net, cfg.Addr)
		}
		if got, want := c.Redacted(), "u@unix(/tmp/mysql.sock)/app"; got != want {
			t.Errorf("Redacted = %q, want %q", got, want)
		}
	})
	t.Run("option file socket", func(t *testing.T) {
		path := writeOptionFile(t, "[client]\nuser=u\nsocket=/var/run/mysqld.sock\n")
		c, err := Resolve(Flags{DefaultsFile: path}, envOf(nil))
		if err != nil {
			t.Fatal(err)
		}
		if c.Socket != "/var/run/mysqld.sock" {
			t.Errorf("socket = %q", c.Socket)
		}
	})
	t.Run("host flag overrides option file socket", func(t *testing.T) {
		path := writeOptionFile(t, "[client]\nuser=u\nsocket=/var/run/mysqld.sock\n")
		c, err := Resolve(Flags{DefaultsFile: path, Host: strp("db.example")}, envOf(nil))
		if err != nil {
			t.Fatal(err)
		}
		if c.Socket != "" || c.Host != "db.example" {
			t.Errorf("socket=%q host=%q", c.Socket, c.Host)
		}
	})
}

func TestOptionFileParsing(t *testing.T) {
	tests := []struct {
		name    string
		content string
		user    string
		pw      string
		host    string
		port    int
	}{
		{
			name:    "plain key=value",
			content: "[client]\nuser=bob\npassword=pw1\nhost=h1\nport=3307\n",
			user:    "bob", pw: "pw1", host: "h1", port: 3307,
		},
		{
			name:    "spaces around equals and indentation",
			content: "[client]\n  user = bob \n\tpassword =  pw two\n",
			user:    "bob", pw: "pw two", host: "127.0.0.1", port: 3306,
		},
		{
			name:    "single quotes are literal",
			content: "[client]\nuser=bob\npassword='a\\\"b # c'\n",
			user:    "bob", pw: `a\"b # c`, host: "127.0.0.1", port: 3306,
		},
		{
			name:    "double quotes with escapes",
			content: "[client]\nuser=bob\npassword=\"q\\\"x\\\\y=z\"\n",
			user:    "bob", pw: `q"x\y=z`, host: "127.0.0.1", port: 3306,
		},
		{
			name:    "distinctive password in double quotes",
			content: "[client]\nuser=bob\npassword=\"" + testPassword + "\"\n",
			user:    "bob", pw: testPassword, host: "127.0.0.1", port: 3306,
		},
		{
			name: "comments, other sections and unknown keys are ignored",
			content: "# comment\n; another\n[mysqld]\nuser=server\npassword=serverpw\n" +
				"[client]\n# user=commented\nuser=bob\nssl-mode=REQUIRED\ndefault-character-set=utf8mb4\nno-beep\n" +
				"[mysql]\nhost=other\n",
			user: "bob", pw: "", host: "127.0.0.1", port: 3306,
		},
		{
			name:    "section names are case-insensitive and repeated sections merge",
			content: "[CLIENT]\nuser=first\n[mysqldump]\nuser=x\n[client]\nuser=second\npassword=pw\n",
			user:    "second", pw: "pw", host: "127.0.0.1", port: 3306,
		},
		{
			name:    "CRLF line endings",
			content: "[client]\r\nuser=bob\r\npassword=pw\r\n",
			user:    "bob", pw: "pw", host: "127.0.0.1", port: 3306,
		},
		{
			name:    "file written by dump-schema.sh",
			content: "[client]\npassword=\"" + `back\\slash\"quote` + "\"\n",
			user:    "", pw: `back\slash"quote`, host: "127.0.0.1", port: 3306,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags := Flags{DefaultsFile: writeOptionFile(t, tt.content)}
			if tt.user == "" {
				flags.User = strp("flaguser")
				tt.user = "flaguser"
			}
			c, err := Resolve(flags, envOf(nil))
			if err != nil {
				t.Fatal(err)
			}
			if c.User != tt.user || c.password != tt.pw || c.Host != tt.host || c.Port != tt.port {
				t.Errorf("got user=%q pw=%q host=%q port=%d, want %q %q %q %d",
					c.User, c.password, c.Host, c.Port, tt.user, tt.pw, tt.host, tt.port)
			}
		})
	}
}

func TestOptionFileErrors(t *testing.T) {
	t.Run("missing file mentions the path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing.cnf")
		_, err := Resolve(Flags{User: strp("u"), DefaultsFile: path}, envOf(nil))
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("err = %v, want it to contain %q", err, path)
		}
	})
	t.Run("invalid port mentions the path and does not leak the password", func(t *testing.T) {
		path := writeOptionFile(t, "[client]\nuser=u\npassword="+testPassword+"\nport=abc\n")
		_, err := Resolve(Flags{DefaultsFile: path}, envOf(nil))
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "port") {
			t.Fatalf("err = %v", err)
		}
		assertNoPassword(t, "error", err.Error())
	})
	t.Run("unterminated quote does not leak the password", func(t *testing.T) {
		path := writeOptionFile(t, "[client]\nuser=u\npassword=\""+testPassword+"\n")
		_, err := Resolve(Flags{DefaultsFile: path}, envOf(nil))
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("err = %v", err)
		}
		assertNoPassword(t, "error", err.Error())
	})
	t.Run("port flag out of range", func(t *testing.T) {
		_, err := Resolve(Flags{User: strp("u"), Port: intp(70000)}, envOf(nil))
		if err == nil || !strings.Contains(err.Error(), "port") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestOptionFilePermissionWarning(t *testing.T) {
	tests := []struct {
		mode     os.FileMode
		wantWarn bool
	}{
		{0o600, false},
		{0o400, false},
		{0o644, true},
		{0o640, true},
		{0o604, true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%04o", tt.mode), func(t *testing.T) {
			path := writeOptionFile(t, "[client]\nuser=u\npassword="+testPassword+"\n")
			if err := os.Chmod(path, tt.mode); err != nil {
				t.Fatal(err)
			}
			c, err := Resolve(Flags{DefaultsFile: path}, envOf(nil))
			if err != nil {
				t.Fatal(err)
			}
			if got := len(c.Warnings) > 0; got != tt.wantWarn {
				t.Fatalf("warnings = %v, want warning: %v", c.Warnings, tt.wantWarn)
			}
			for _, w := range c.Warnings {
				if !strings.Contains(w, path) {
					t.Errorf("warning should mention the path: %q", w)
				}
				assertNoPassword(t, "warning", w)
			}
		})
	}
}

func TestRedacted(t *testing.T) {
	tests := []struct {
		c    Config
		want string
	}{
		{Config{User: "u", Host: "db.example", Port: 3306, Database: "app", password: testPassword}, "u@db.example:3306/app"},
		{Config{User: "u", Host: "127.0.0.1", Port: 33057, password: testPassword}, "u@127.0.0.1:33057/"},
		{Config{User: "u", Host: "::1", Port: 3306, Database: "app"}, "u@[::1]:3306/app"},
		{Config{User: "u", Host: "ignored", Port: 1, Socket: "/s.sock", Database: "app", password: testPassword}, "u@unix(/s.sock)/app"},
	}
	for _, tt := range tests {
		got := tt.c.Redacted()
		if got != tt.want {
			t.Errorf("Redacted() = %q, want %q", got, tt.want)
		}
		assertNoPassword(t, "Redacted", got)
	}
}

func TestConfigFormattingHidesPassword(t *testing.T) {
	c := Config{User: "u", Host: "h", Port: 3306, Database: "app", password: testPassword}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		assertNoPassword(t, verb, fmt.Sprintf(verb, c))
		assertNoPassword(t, verb+" pointer", fmt.Sprintf(verb, &c))
	}
}

func TestDSN(t *testing.T) {
	c := Config{User: "u", Host: "db.example", Port: 3307, Database: "app", password: testPassword}
	cfg, err := mysql.ParseDSN(c.DSN())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Net != "tcp" || cfg.Addr != "db.example:3307" || cfg.User != "u" || cfg.Passwd != testPassword || cfg.DBName != "app" {
		t.Errorf("unexpected config: net=%q addr=%q user=%q db=%q", cfg.Net, cfg.Addr, cfg.User, cfg.DBName)
	}
	if cfg.ParseTime {
		t.Error("parseTime should be false")
	}
	if cfg.InterpolateParams {
		t.Error("interpolateParams should be false")
	}
	if cfg.Collation != "utf8mb4_unicode_ci" {
		t.Errorf("collation = %q", cfg.Collation)
	}
	if cfg.Timeout <= 0 {
		t.Errorf("timeout = %v, want > 0", cfg.Timeout)
	}
}

// TestOpenErrorHidesPassword connects to a port nobody listens on. It needs no
// MySQL server.
func TestOpenErrorHidesPassword(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close() // nothing listens on port now

	c := Config{User: "u", Host: "127.0.0.1", Port: port, Database: "app", password: testPassword}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := Open(ctx, c)
	if err == nil {
		db.Close()
		t.Fatal("expected a connection error")
	}
	msg := err.Error()
	if !strings.Contains(msg, c.Redacted()) {
		t.Errorf("error should contain %q: %s", c.Redacted(), msg)
	}
	assertNoPassword(t, "error", msg)
	if strings.Contains(msg, c.DSN()) {
		t.Errorf("error contains the DSN: %s", msg)
	}
}

func TestScrubPassword(t *testing.T) {
	c := Config{User: "u", Host: "h", Port: 1, password: testPassword}
	err := c.connectError(fmt.Errorf("driver said %s", testPassword))
	assertNoPassword(t, "error", err.Error())
	if !strings.Contains(err.Error(), c.Redacted()) {
		t.Errorf("error should contain Redacted: %v", err)
	}
}
