// open-vault-cbt-agent serves a fixed privileged CBT workflow over a Unix socket.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/ichenwin/open-vault/internal/cbtservice"
)

func run(ctx context.Context, args []string) error {
	f := flag.NewFlagSet("open-vault-cbt-agent", flag.ContinueOnError)
	config := f.String("config", "/etc/open-vault/cbt-agent.json", "absolute startup configuration path")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("usage: open-vault-cbt-agent -config /etc/open-vault/cbt-agent.json")
	}
	c, err := cbtservice.LoadConfig(*config)
	if err != nil {
		return err
	}
	s, err := cbtservice.New(ctx, c)
	if err != nil {
		return err
	}
	return s.Serve(ctx)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		_ = json.NewEncoder(os.Stderr).Encode(map[string]string{"error": err.Error()})
		os.Exit(1)
	}
}
