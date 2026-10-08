//go:build integration

package dbconn

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/yamagame/relation-deleter/internal/testutil/mysqltest"
)

func targetFlags(tg mysqltest.Target) Flags {
	return Flags{
		Host:     strp(tg.Host),
		Port:     intp(tg.Port),
		User:     strp(tg.User),
		Database: strp(tg.DB),
	}
}

func openCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestIntegrationOpen(t *testing.T) {
	mysqltest.ForEach(t, func(t *testing.T, tg mysqltest.Target, _ *sql.DB) {
		t.Run("MYSQL_PWD succeeds", func(t *testing.T) {
			c, err := Resolve(targetFlags(tg), envOf(map[string]string{PasswordEnv: tg.Password}))
			if err != nil {
				t.Fatal(err)
			}
			db, err := Open(openCtx(t), c)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var got string
			if err := db.QueryRow("SELECT DATABASE()").Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != tg.DB {
				t.Errorf("DATABASE() = %q, want %q", got, tg.DB)
			}
		})

		t.Run("option file succeeds", func(t *testing.T) {
			path := writeOptionFile(t, fmt.Sprintf("[client]\nuser=%s\npassword=\"%s\"\nhost=%s\nport=%d\n",
				tg.User, tg.Password, tg.Host, tg.Port))
			c, err := Resolve(Flags{DefaultsFile: path, Database: strp(tg.DB)}, envOf(nil))
			if err != nil {
				t.Fatal(err)
			}
			db, err := Open(openCtx(t), c)
			if err != nil {
				t.Fatal(err)
			}
			db.Close()
		})

		t.Run("wrong password hides the password", func(t *testing.T) {
			c, err := Resolve(targetFlags(tg), envOf(map[string]string{PasswordEnv: testPassword}))
			if err != nil {
				t.Fatal(err)
			}
			db, err := Open(openCtx(t), c)
			if err == nil {
				db.Close()
				t.Fatal("expected an authentication error")
			}
			msg := err.Error()
			if !strings.Contains(msg, c.Redacted()) {
				t.Errorf("error should contain %q: %s", c.Redacted(), msg)
			}
			assertNoPassword(t, "error", msg)
			if !strings.Contains(msg, "1045") {
				t.Errorf("error should carry the driver's access-denied error: %s", msg)
			}
		})

		t.Run("wrong port mentions host:port", func(t *testing.T) {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := l.Addr().(*net.TCPAddr).Port
			l.Close()

			flags := targetFlags(tg)
			flags.Port = intp(port)
			c, err := Resolve(flags, envOf(map[string]string{PasswordEnv: testPassword}))
			if err != nil {
				t.Fatal(err)
			}
			_, err = Open(openCtx(t), c)
			if err == nil {
				t.Fatal("expected a connection error")
			}
			if want := fmt.Sprintf("%s:%d", tg.Host, port); !strings.Contains(err.Error(), want) {
				t.Errorf("error should mention %q: %v", want, err)
			}
			assertNoPassword(t, "error", err.Error())
		})
	})
}
