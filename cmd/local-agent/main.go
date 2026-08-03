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
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals
		cancel()
		<-signals
		// A second signal bypasses the drain period but still lets durable workers
		// classify interrupted operations before process exit.
		application.ForceShutdown()
	}()
	code := cli.Execute(ctx, root, os.Args[1:], os.Stderr)
	signal.Stop(signals)
	cancel()
	os.Exit(code)
}
