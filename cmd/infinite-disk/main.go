package main

import (
	"context"
	"errors"
	"fmt"
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
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "\nCancelled. No further actions will start; check receipts if an action had begun.")
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
