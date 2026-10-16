package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/omniaura/infinite-disk/internal/cli"
)

var version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := cli.New(version).ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
