package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/yamagame/mysql-relation-deleter/internal/execute"
)

// countingReader records whether the flow read stdin.
type countingReader struct {
	r     io.Reader
	reads int
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.reads++
	return c.r.Read(p)
}

// assertExecuteTx checks that the execute flow opened exactly one read-write
// transaction with a context that is not cancellable, committed commits
// times, rolled back rollbacks times and closed the connection.
func assertExecuteTx(t *testing.T, e *flowEnv, commits, rollbacks int) {
	t.Helper()
	if len(e.db.opts) != 1 {
		t.Fatalf("BeginTx called %d times, want 1", len(e.db.opts))
	}
	if o := e.db.opts[0]; o != nil && o.ReadOnly {
		t.Errorf("BeginTx opts = %+v, want a read-write transaction", o)
	}
	if e.db.ctxs[0].Done() != nil {
		t.Errorf("BeginTx got a cancellable context; want context.WithoutCancel")
	}
	tx := e.db.tx
	if tx.commits != commits {
		t.Errorf("Commit called %d times, want %d", tx.commits, commits)
	}
	if tx.rollbacks != rollbacks {
		t.Errorf("Rollback called %d times, want %d", tx.rollbacks, rollbacks)
	}
	if tx.execsAfterCommit != 0 {
		t.Errorf("%d statements issued after Commit", tx.execsAfterCommit)
	}
	if e.db.closed != 1 {
		t.Errorf("Close called %d times, want 1", e.db.closed)
	}
}

func assertNoDelete(t *testing.T, e *flowEnv) {
	t.Helper()
	if n := e.db.tx.execs; n != 0 {
		t.Errorf("ExecContext called %d times, want 0: %q", n, e.db.tx.stmts)
	}
}

const prompt = "Delete 3 records from 2 tables? [y/N]: "

func TestExecuteApproved(t *testing.T) {
	for _, answer := range []string{"y\n", "yes\n", "Y\n", "YES\n", "  Yes  \n", "y", "y\r\n"} {
		t.Run(strings.TrimSpace(answer), func(t *testing.T) {
			e := newFlowEnv()
			e.tty = true
			e.in = strings.NewReader(answer)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := e.run(ctx, flowArgs(t, "--id", "1", "--execute")...)
			if r.code != 0 {
				t.Fatalf("exit code = %d, want 0\nstderr:\n%s", r.code, r.errOut)
			}
			assertExecuteTx(t, e, 1, 0)
			stmts := e.db.tx.stmts
			if len(stmts) != 2 || !strings.HasPrefix(stmts[0], "DELETE FROM `orders`") || !strings.HasPrefix(stmts[1], "DELETE FROM `users`") {
				t.Errorf("statements = %q, want orders then users", stmts)
			}
			wantOut := "Rows to delete:\n  orders: 2\n  users: 1\ntotal=3 tables=2\n" +
				"Deleted rows:\n  orders: 2\n  users: 1\ntotal=3 tables=2\n"
			if r.out != wantOut {
				t.Errorf("stdout =\n%s\nwant\n%s", r.out, wantOut)
			}
			if !strings.Contains(r.errOut, prompt) {
				t.Errorf("stderr lacks the prompt %q:\n%s", prompt, r.errOut)
			}
			for _, w := range []string{"collect users: 1 rows", "delete orders: 2 rows", "delete users: 1 rows"} {
				if !strings.Contains(r.errOut, w) {
					t.Errorf("stderr lacks %q:\n%s", w, r.errOut)
				}
			}
		})
	}
}

func TestExecuteDeclined(t *testing.T) {
	for name, answer := range map[string]string{
		"n": "n\n", "no": "no\n", "empty line": "\n", "EOF": "", "other": "sure\n", "y after newline": "\ny\n",
	} {
		t.Run(name, func(t *testing.T) {
			e := newFlowEnv()
			e.tty = true
			e.in = strings.NewReader(answer)
			r := e.run(context.Background(), flowArgs(t, "--id", "1", "--execute")...)
			if r.code != 3 {
				t.Fatalf("exit code = %d, want 3\nstderr:\n%s", r.code, r.errOut)
			}
			assertExecuteTx(t, e, 0, 1)
			assertNoDelete(t, e)
			if !strings.Contains(r.out, "total=3 tables=2") {
				t.Errorf("summary not printed:\n%s", r.out)
			}
			if !strings.HasSuffix(r.errOut, "relation-deleter: aborted; nothing deleted\n") {
				t.Errorf("stderr = %q", r.errOut)
			}
		})
	}
}

func TestExecuteNonInteractive(t *testing.T) {
	t.Run("without --yes", func(t *testing.T) {
		e := newFlowEnv()
		in := &countingReader{r: strings.NewReader("y\n")}
		e.in = in
		r := e.run(context.Background(), flowArgs(t, "--id", "1", "--execute")...)
		if r.code != 3 {
			t.Fatalf("exit code = %d, want 3\nstderr:\n%s", r.code, r.errOut)
		}
		want := "relation-deleter: refusing to delete without confirmation on a non-interactive input; use --yes\n"
		if r.errOut != want {
			t.Errorf("stderr = %q, want %q", r.errOut, want)
		}
		if len(e.db.opts) != 0 || e.sources != 0 || in.reads != 0 {
			t.Errorf("BeginTx = %d, NewRowSource = %d, stdin reads = %d; want 0, 0, 0", len(e.db.opts), e.sources, in.reads)
		}
		if r.out != "" {
			t.Errorf("stdout = %q, want empty", r.out)
		}
		if e.db.closed != 1 {
			t.Errorf("Close called %d times, want 1", e.db.closed)
		}
	})
	t.Run("with --yes", func(t *testing.T) {
		e := newFlowEnv()
		in := &countingReader{r: strings.NewReader("n\n")}
		e.in = in
		r := e.run(context.Background(), flowArgs(t, "--id", "1", "--execute", "--yes")...)
		if r.code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr:\n%s", r.code, r.errOut)
		}
		assertExecuteTx(t, e, 1, 0)
		if in.reads != 0 {
			t.Errorf("stdin read %d times with --yes", in.reads)
		}
		if strings.Contains(r.errOut, "[y/N]") {
			t.Errorf("prompt printed with --yes:\n%s", r.errOut)
		}
		if !strings.Contains(r.out, "Deleted rows:") {
			t.Errorf("result not printed:\n%s", r.out)
		}
	})
}

func TestExecuteYesOnTerminal(t *testing.T) {
	e := newFlowEnv()
	e.tty = true
	in := &countingReader{r: strings.NewReader("n\n")}
	e.in = in
	r := e.run(context.Background(), flowArgs(t, "--id", "1", "--execute", "--yes")...)
	if r.code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr:\n%s", r.code, r.errOut)
	}
	assertExecuteTx(t, e, 1, 0)
	if in.reads != 0 || strings.Contains(r.errOut, "[y/N]") {
		t.Errorf("prompted with --yes (reads = %d):\n%s", in.reads, r.errOut)
	}
	if !strings.Contains(r.out, "Rows to delete:") || !strings.Contains(r.out, "Deleted rows:") {
		t.Errorf("stdout lacks the summary or the result:\n%s", r.out)
	}
}

func TestExecuteFailures(t *testing.T) {
	t.Run("delete error", func(t *testing.T) {
		e := newFlowEnv()
		e.db.tx.execErr = map[string]error{"`users`": errors.New("Error 1451: Cannot delete or update a parent row")}
		r := e.run(context.Background(), flowArgs(t, "--id", "1", "--execute", "--yes")...)
		if r.code != 1 {
			t.Fatalf("exit code = %d, want 1\nstderr:\n%s", r.code, r.errOut)
		}
		assertExecuteTx(t, e, 0, 1)
		want := "relation-deleter: failed at table users: delete failed: Error 1451: Cannot delete or update a parent row; all changes rolled back\n"
		if !strings.HasSuffix(r.errOut, want) {
			t.Errorf("stderr = %q, want suffix %q", r.errOut, want)
		}
		if strings.Contains(r.out, "Deleted rows:") {
			t.Errorf("result printed after a failure:\n%s", r.out)
		}
	})
	t.Run("count mismatch", func(t *testing.T) {
		e := newFlowEnv()
		e.db.tx.affected = map[string]int64{"`orders`": 1}
		r := e.run(context.Background(), flowArgs(t, "--id", "1", "--execute", "--yes")...)
		if r.code != 1 {
			t.Fatalf("exit code = %d, want 1\nstderr:\n%s", r.code, r.errOut)
		}
		assertExecuteTx(t, e, 0, 1)
		for _, w := range []string{
			"relation-deleter: deleted row count does not match the plan\n",
			"relation-deleter:   orders: expected 2, deleted 1\n",
			"relation-deleter: all changes rolled back\n",
		} {
			if !strings.Contains(r.errOut, w) {
				t.Errorf("stderr lacks %q:\n%s", w, r.errOut)
			}
		}
		if strings.Contains(r.errOut, "users: expected") {
			t.Errorf("stderr lists a matching table:\n%s", r.errOut)
		}
		if strings.Contains(r.out, "Deleted rows:") {
			t.Errorf("result printed after a mismatch:\n%s", r.out)
		}
	})
	t.Run("commit error", func(t *testing.T) {
		e := newFlowEnv()
		e.db.tx.commitErr = errors.New("driver: bad connection")
		r := e.run(context.Background(), flowArgs(t, "--id", "1", "--execute", "--yes")...)
		if r.code != 1 {
			t.Fatalf("exit code = %d, want 1\nstderr:\n%s", r.code, r.errOut)
		}
		if !strings.Contains(r.errOut, "relation-deleter: commit: driver: bad connection") {
			t.Errorf("stderr = %q", r.errOut)
		}
		if strings.Contains(r.out, "Deleted rows:") {
			t.Errorf("result printed after a failed commit:\n%s", r.out)
		}
		if e.db.tx.commits != 1 || e.db.closed != 1 {
			t.Errorf("commits = %d, closed = %d; want 1, 1", e.db.tx.commits, e.db.closed)
		}
	})
	t.Run("BeginTx error", func(t *testing.T) {
		e := newFlowEnv()
		e.db.beginErr = errors.New("driver: bad connection")
		r := e.run(context.Background(), flowArgs(t, "--id", "1", "--execute", "--yes")...)
		if r.code != 1 || !strings.Contains(r.errOut, "bad connection") {
			t.Fatalf("exit code = %d, stderr = %q; want 1 and the error", r.code, r.errOut)
		}
		if e.sources != 0 || e.db.closed != 1 {
			t.Errorf("sources = %d, closed = %d; want 0, 1", e.sources, e.db.closed)
		}
	})
	t.Run("collect error", func(t *testing.T) {
		e := newFlowEnv()
		e.src.err = map[string]error{"orders": errors.New("lock wait timeout")}
		r := e.run(context.Background(), flowArgs(t, "--id", "1", "--execute", "--yes")...)
		if r.code != 1 {
			t.Fatalf("exit code = %d, want 1\nstderr:\n%s", r.code, r.errOut)
		}
		assertExecuteTx(t, e, 0, 1)
		assertNoDelete(t, e)
	})
}

func TestExecuteStopsBeforeConfirm(t *testing.T) {
	t.Run("limit exceeded", func(t *testing.T) {
		e := newFlowEnv()
		e.tty = true
		in := &countingReader{r: strings.NewReader("y\n")}
		e.in = in
		r := e.run(context.Background(), flowArgs(t, "--id", "1", "--execute", "--max-records", "2")...)
		if r.code != 3 {
			t.Fatalf("exit code = %d, want 3\nstderr:\n%s", r.code, r.errOut)
		}
		assertExecuteTx(t, e, 0, 1)
		assertNoDelete(t, e)
		if in.reads != 0 || r.out != "" {
			t.Errorf("stdin reads = %d, stdout = %q; want 0 and empty", in.reads, r.out)
		}
		if !strings.HasSuffix(r.errOut, "relation-deleter: 3 records exceed --max-records 2; nothing deleted\n") {
			t.Errorf("stderr = %q", r.errOut)
		}
	})
	t.Run("all roots missing", func(t *testing.T) {
		e := newFlowEnv()
		r := e.run(context.Background(), flowArgs(t, "--id", "98", "--execute", "--yes")...)
		if r.code != 2 {
			t.Fatalf("exit code = %d, want 2\nstderr:\n%s", r.code, r.errOut)
		}
		assertExecuteTx(t, e, 0, 1)
		assertNoDelete(t, e)
		if r.out != "" {
			t.Errorf("stdout = %q, want empty", r.out)
		}
	})
}

func TestConfirm(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"y\n", true}, {"Y\n", true}, {"yes\n", true}, {"YeS\n", true}, {" y \n", true}, {"y", true},
		{"n\n", false}, {"\n", false}, {"", false}, {"yy\n", false}, {"no\n", false}, {"\ny\n", false},
	}
	for _, tt := range tests {
		var out strings.Builder
		got := confirm(strings.NewReader(tt.in), &out, 38, 16)
		if got != tt.want {
			t.Errorf("confirm(%q) = %v, want %v", tt.in, got, tt.want)
		}
		if want := "Delete 38 records from 16 tables? [y/N]: "; out.String() != want {
			t.Errorf("prompt = %q, want %q", out.String(), want)
		}
	}
}

func TestExecuteFailureMessages(t *testing.T) {
	ee := &execute.ExecError{Table: "orders", Err: errors.New("delete failed: boom")}
	restore := errors.New("re-enable foreign_key_checks after group ending at table users: bad connection")
	tests := []struct {
		name string
		err  error
		want []string
	}{
		{"exec error", ee, []string{"failed at table orders: delete failed: boom; all changes rolled back"}},
		{"joined with restore error", errors.Join(ee, restore), []string{
			"failed at table orders: table orders: delete failed: boom\n" + restore.Error() + "; all changes rolled back"}},
		{"other", errors.New("boom"), []string{"delete failed: boom; all changes rolled back"}},
	}
	for _, tt := range tests {
		got := executeFailure(tt.err)
		if strings.Join(got, "|") != strings.Join(tt.want, "|") {
			t.Errorf("%s: executeFailure = %q, want %q", tt.name, got, tt.want)
		}
	}
}
