// Command relation-deleter deletes a row together with the rows that depend on
// it, following foreign keys and manually defined relations.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/yamagame/relation-deleter/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], cli.StdIO(), cli.DefaultDeps())
	stop()
	os.Exit(code)
}
