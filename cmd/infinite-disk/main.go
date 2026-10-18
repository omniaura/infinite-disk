package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/omniaura/infinite-disk/internal/cli"
)

var version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// Restore default signal behavior after the first interrupt, so a second
	// Ctrl+C can terminate a stalled external operation as users expect.
	go func() { <-ctx.Done(); cancel() }()
	command := cli.New(version)
	command.SilenceErrors = true
	if err := command.ExecuteContext(ctx); err != nil {
		asJSON, _ := command.PersistentFlags().GetBool("json")
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			cli.WriteError(os.Stderr, err, asJSON, true)
			os.Exit(130)
		}
		cli.WriteError(os.Stderr, err, asJSON, false)
		os.Exit(1)
	}
}
