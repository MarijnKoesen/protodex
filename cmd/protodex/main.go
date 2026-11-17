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
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/marijnkoesen/protodex/internal/auth"
	"github.com/marijnkoesen/protodex/internal/config"
	"github.com/marijnkoesen/protodex/internal/owner"
	"github.com/marijnkoesen/protodex/internal/server"
	"github.com/marijnkoesen/protodex/internal/store"
	"github.com/marijnkoesen/protodex/internal/store/sqlite"
	"github.com/marijnkoesen/protodex/internal/version"
)

const usage = `usage: protodex <command> [flags]

commands:
  serve      run the registry server
  bootstrap  create the first admin user (printing its initial token), the
             instance organization and reserved mirror owners
  owner      create, list or rename users, organizations and mirror owners
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
	case "owner":
		return ownerCmd(ctx, args[1:], os.Getenv, stdout, stderr)
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

// bootstrap implements `protodex bootstrap [-db path] [-user name] [-org name]
// [-mirror-owners a,b]`. It creates the first user and an initial token (from
// PROTODEX_BOOTSTRAP_TOKEN, or newly generated) if no user exists yet. A
// generated token is printed once to stdout and never logged; a supplied one
// is not echoed. The instance organization and mirror owners are created if
// missing; rerunning is safe.
func bootstrap(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", getenv("PROTODEX_DB_PATH"), "SQLite database path (env PROTODEX_DB_PATH)")
	user := fs.String("user", cmp.Or(getenv("PROTODEX_BOOTSTRAP_USER"), "admin"), "admin user name (env PROTODEX_BOOTSTRAP_USER)")
	org := fs.String("org", getenv("PROTODEX_BOOTSTRAP_ORG"), "instance organization to create (env PROTODEX_BOOTSTRAP_ORG)")
	mirrors := fs.String("mirror-owners", getenv("PROTODEX_MIRROR_OWNERS"),
		"comma-separated upstream owner names to reserve for mirrors, e.g. googleapis,bufbuild (env PROTODEX_MIRROR_OWNERS)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("bootstrap: unexpected arguments %q", fs.Args())
	}
	if *dbPath == "" {
		return errors.New("bootstrap: -db or PROTODEX_DB_PATH is required")
	}
	if err := owner.ValidateName(owner.User, *user); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	type ownerSpec struct {
		kind owner.Kind
		name string
	}
	var ensure []ownerSpec
	if *org != "" {
		ensure = append(ensure, ownerSpec{owner.Organization, *org})
	}
	for m := range strings.SplitSeq(*mirrors, ",") {
		if m = strings.TrimSpace(m); m != "" {
			ensure = append(ensure, ownerSpec{owner.Mirror, m})
		}
	}
	for _, e := range ensure {
		if err := owner.ValidateName(e.kind, e.name); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
	}

	s, err := sqlite.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	// Owners first: if one conflicts, no token has been issued yet.
	var created []string
	err = s.WithTx(ctx, func(q store.Querier) error {
		created = nil
		for _, e := range ensure {
			_, ok, err := owner.Ensure(ctx, q, e.kind, e.name)
			if err != nil {
				return err
			}
			if ok {
				created = append(created, fmt.Sprintf("%s %q", kindLabel(e.kind), e.name))
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	for _, c := range created {
		if _, err := fmt.Fprintf(stdout, "bootstrap: created %s\n", c); err != nil {
			return err
		}
	}

	supplied := getenv("PROTODEX_BOOTSTRAP_TOKEN")
	secret, userCreated, err := auth.Bootstrap(ctx, s, *user, supplied)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	switch {
	case !userCreated:
		_, err = fmt.Fprintln(stdout, "bootstrap: a user already exists; no user created")
	case supplied != "":
		_, err = fmt.Fprintf(stdout, "bootstrap: created user %q with the token from PROTODEX_BOOTSTRAP_TOKEN\n", *user)
	default:
		_, err = fmt.Fprintf(stdout, "bootstrap: created user %q. Token (shown once, store it safely):\n%s\n", *user, secret)
	}
	return err
}

func kindLabel(k owner.Kind) string {
	switch k {
	case owner.User:
		return "user"
	case owner.Mirror:
		return "mirror owner"
	}
	return "organization"
}

const ownerUsage = `usage: protodex owner <action> [-db path] [args]

actions:
  create user NAME...    create users, printing an initial token for each
  create org NAME...     create organizations
  create mirror NAME...  reserve upstream owner names (e.g. googleapis) for mirrors
  list                   list all owners
  rename OLD NEW         rename a user or organization (mirrors cannot be renamed)
`

// ownerCmd implements `protodex owner`.
func ownerCmd(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, ownerUsage)
		return errors.New("owner: missing action")
	}
	action := args[0]
	fs := flag.NewFlagSet("owner "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, ownerUsage); fs.PrintDefaults() }
	dbPath := fs.String("db", getenv("PROTODEX_DB_PATH"), "SQLite database path (env PROTODEX_DB_PATH)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()

	var run func(st store.Store) error
	switch action {
	case "create":
		if len(rest) < 2 {
			return errors.New("owner create: want a kind (user, org or mirror) and at least one name")
		}
		kind, err := owner.ParseKind(rest[0])
		if err != nil {
			return fmt.Errorf("owner create: %w", err)
		}
		run = func(st store.Store) error { return createOwners(ctx, st, kind, rest[1:], stdout) }
	case "list":
		if len(rest) != 0 {
			return errors.New("owner list: takes no arguments")
		}
		run = func(st store.Store) error { return listOwners(ctx, st, stdout) }
	case "rename":
		if len(rest) != 2 {
			return errors.New("owner rename: want OLD and NEW names")
		}
		run = func(st store.Store) error {
			o, err := owner.Rename(ctx, st, rest[0], rest[1])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "renamed %s %q to %q\n", kindLabel(owner.KindOf(o)), rest[0], o.Name)
			return err
		}
	case "help", "-h", "-help", "--help":
		_, err := fmt.Fprint(stdout, ownerUsage)
		return err
	default:
		_, _ = fmt.Fprint(stderr, ownerUsage)
		return fmt.Errorf("owner: unknown action %q", action)
	}

	if *dbPath == "" {
		return fmt.Errorf("owner %s: -db or PROTODEX_DB_PATH is required", action)
	}
	st, err := sqlite.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("owner %s: %w", action, err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("owner %s: %w", action, err)
	}
	if err := run(st); err != nil {
		return fmt.Errorf("owner %s: %w", action, err)
	}
	return nil
}

// createOwners creates all names atomically. Users get an initial token,
// printed once.
func createOwners(ctx context.Context, st store.Store, kind owner.Kind, names []string, stdout io.Writer) error {
	for _, n := range names {
		if err := owner.ValidateName(kind, n); err != nil {
			return err
		}
	}
	secrets := make([]string, len(names))
	err := st.WithTx(ctx, func(q store.Querier) error {
		for i, n := range names {
			o, err := owner.Create(ctx, q, kind, n)
			if err != nil {
				return err
			}
			if kind != owner.User {
				continue
			}
			if secrets[i], err = auth.GenerateSecret(); err != nil {
				return err
			}
			if _, err := q.CreateToken(ctx, store.Token{OwnerID: o.ID, Name: "initial", Hash: auth.HashSecret(secrets[i])}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i, n := range names {
		if kind == owner.User {
			_, err = fmt.Fprintf(stdout, "created user %q. Token (shown once, store it safely):\n%s\n", n, secrets[i])
		} else {
			_, err = fmt.Fprintf(stdout, "created %s %q\n", kindLabel(kind), n)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func listOwners(ctx context.Context, st store.Store, stdout io.Writer) error {
	owners, err := st.ListOwners(ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tKIND\tID\tCREATED")
	for _, o := range owners {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", o.Name, kindLabel(owner.KindOf(o)), o.ID, o.CreatedAt.Format(time.RFC3339))
	}
	return tw.Flush()
}
