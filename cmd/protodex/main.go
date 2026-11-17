// SPDX-License-Identifier: Apache-2.0

// Command protodex is the Protodex server.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/marijnkoesen/protodex/internal/auth"
	"github.com/marijnkoesen/protodex/internal/config"
	"github.com/marijnkoesen/protodex/internal/server"
	"github.com/marijnkoesen/protodex/internal/store/sqlite"
	"github.com/marijnkoesen/protodex/internal/version"
)

const usage = `usage: protodex <command> [flags]

commands:
  serve      run the registry server
  bootstrap  create the first admin user and print its initial token
  migrate    apply (or "down": roll back one) database migrations
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
		var opts []server.Option
		if cfg.DBPath != "" {
			// Holding the store open keeps the exclusive lock for the
			// server's lifetime; a second server on the same DB is refused.
			st, err := sqlite.Open(cfg.DBPath)
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer func() { _ = st.Close() }()
			opts = append(opts, server.WithStore(st))
		}
		ln, err := net.Listen("tcp", cfg.Addr)
		if err != nil {
			return err
		}
		return server.Serve(ctx, cfg, cfg.Logger(stderr), ln, opts...)
	case "bootstrap":
		return bootstrap(ctx, args[1:], os.Getenv, stdout, stderr)
	case "migrate":
		return migrate(ctx, args[1:], os.Getenv, stdout, stderr)
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

// migrate implements `protodex migrate [-db path] [up|down]`.
func migrate(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", getenv("PROTODEX_DB_PATH"), "SQLite database path (env PROTODEX_DB_PATH)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" {
		return errors.New("migrate: -db or PROTODEX_DB_PATH is required")
	}
	direction := "up"
	if fs.NArg() > 0 {
		direction = fs.Arg(0)
	}
	s, err := sqlite.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer func() { _ = s.Close() }()
	switch direction {
	case "up":
		err = s.Migrate(ctx)
	case "down":
		err = s.Rollback(ctx)
	default:
		return fmt.Errorf("migrate: unknown direction %q (want up or down)", direction)
	}
	if err != nil {
		return fmt.Errorf("migrate %s: %w", direction, err)
	}
	_, err = fmt.Fprintf(stdout, "migrate %s: ok\n", direction)
	return err
}

// bootstrap implements `protodex bootstrap [-db path] [-user name]`. It creates
// the first user and an initial token (from PROTODEX_BOOTSTRAP_TOKEN, or newly
// generated) if no user exists yet. A generated token is printed once to
// stdout and never logged; a supplied one is not echoed.
func bootstrap(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", getenv("PROTODEX_DB_PATH"), "SQLite database path (env PROTODEX_DB_PATH)")
	user := fs.String("user", cmp.Or(getenv("PROTODEX_BOOTSTRAP_USER"), "admin"), "admin user name (env PROTODEX_BOOTSTRAP_USER)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" {
		return errors.New("bootstrap: -db or PROTODEX_DB_PATH is required")
	}
	s, err := sqlite.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	supplied := getenv("PROTODEX_BOOTSTRAP_TOKEN")
	secret, created, err := auth.Bootstrap(ctx, s, *user, supplied)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	switch {
	case !created:
		_, err = fmt.Fprintln(stdout, "bootstrap: a user already exists; nothing to do")
	case supplied != "":
		_, err = fmt.Fprintf(stdout, "bootstrap: created user %q with the token from PROTODEX_BOOTSTRAP_TOKEN\n", *user)
	default:
		_, err = fmt.Fprintf(stdout, "bootstrap: created user %q. Token (shown once, store it safely):\n%s\n", *user, secret)
	}
	return err
}
