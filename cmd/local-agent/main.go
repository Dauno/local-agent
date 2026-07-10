package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Dauno/slack-local-agent/internal/app"
	"github.com/Dauno/slack-local-agent/internal/cli"
)

func main() {
	projectRoot, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "determine current directory:", err)
		os.Exit(2)
	}
	application, err := app.New(projectRoot, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	root, err := cli.NewRoot(application, cli.Streams{In: os.Stdin, Out: os.Stdout, Err: os.Stderr})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Execute(ctx, root, os.Args[1:], os.Stderr)
	cancel()
	os.Exit(code)
}
