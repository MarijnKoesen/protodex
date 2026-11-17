// SPDX-License-Identifier: Apache-2.0

// Command protodex is the Protodex server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/marijnkoesen/protodex/internal/config"
	"github.com/marijnkoesen/protodex/internal/server"
	"github.com/marijnkoesen/protodex/internal/version"
)

const usage = `usage: protodex <command> [flags]

commands:
  serve      run the registry server
  migrate    apply database migrations (not implemented yet)
  version    print the version
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprintln(os.Stderr, "protodex:", err)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return errors.New("missing command")
	}
	switch args[0] {
	case "serve":
		cfg, err := config.Load(args[1:], os.Getenv, stderr)
		if err != nil {
			return err
		}
		ln, err := net.Listen("tcp", cfg.Addr)
		if err != nil {
			return err
		}
		return server.Serve(ctx, cfg, cfg.Logger(stderr), ln)
	case "migrate":
		return errors.New("migrate: not implemented yet")
	case "version", "-version", "--version":
		_, err := fmt.Fprintln(stdout, version.String())
		return err
	case "help", "-h", "-help", "--help":
		_, err := fmt.Fprint(stdout, usage)
		return err
	default:
		_, _ = fmt.Fprint(stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}
