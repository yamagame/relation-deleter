package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// confirm writes the y/N prompt for deleting total rows from tables tables to
// out and reads one line from in. It returns true only for "y" or "yes"
// (case-insensitive, surrounding spaces ignored); anything else, an empty
// line, end of input or a read error is a refusal (6.1, 6.2).
func confirm(in io.Reader, out io.Writer, total int64, tables int) bool {
	fmt.Fprintf(out, "Delete %d records from %d tables? [y/N]: ", total, tables)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// stdinIsTerminal reports whether stdin is an interactive terminal: a
// character device other than the null device (6.4). </dev/null is a
// character device too, but nobody can answer the prompt there.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(fi, null) {
		return false
	}
	return true
}
