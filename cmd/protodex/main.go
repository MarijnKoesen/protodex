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

	"github.com/marijnkoesen/protodex/internal/audit"
	"github.com/marijnkoesen/protodex/internal/auth"
	"github.com/marijnkoesen/protodex/internal/blob/fsblob"
	"github.com/marijnkoesen/protodex/internal/config"
	"github.com/marijnkoesen/protodex/internal/module"
	"github.com/marijnkoesen/protodex/internal/oidcauth"
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
  owner      create, list or rename users, organizations and mirror owners;
             set user roles (viewer, member, admin)
  contributor  grant, revoke or list per-module contributor roles
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
			blobDir := cfg.BlobDir
			if blobDir == "" {
				blobDir = cfg.DBPath + ".blobs"
			}
			blobs, err := fsblob.New(blobDir, 0)
			if err != nil {
				return fmt.Errorf("open blob store: %w", err)
			}
			opts = append(opts, server.WithBlobs(blobs),
				server.WithUploadLimits(server.UploadLimits{MaxFiles: cfg.MaxPushFiles, MaxFileBytes: cfg.MaxPushFileBytes}),
				// SQLite mode is single-process, so GC runs in-process.
				server.WithGC(server.GCConfig{Interval: time.Duration(cfg.GCInterval),
					Grace: time.Duration(cfg.GCGracePeriod), DryRun: cfg.GCDryRun}))
			opts = append(opts, server.WithWebhooks(server.WebhookConfig{Interval: time.Duration(cfg.WebhookInterval),
				Timeout: time.Duration(cfg.WebhookTimeout), MaxAttempts: cfg.WebhookMaxAttempts}))
			if cfg.OIDC.Enabled() {
				opts = append(opts, server.WithOIDC(oidcauth.New(cfg.OIDC, st, cfg.Logger(stderr))))
			}
		} else if cfg.OIDC.Enabled() {
			return errors.New("oidc requires db_path: logins are stored in the database")
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
	case "contributor":
		return contributorCmd(ctx, args[1:], os.Getenv, stdout, stderr)
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
	// Reject one name requested for two different owners before touching
	// the database.
	seen := map[string]owner.Kind{*user: owner.User}
	for _, e := range ensure {
		if err := owner.ValidateName(e.kind, e.name); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
		if k, ok := seen[e.name]; ok && k != e.kind {
			return fmt.Errorf("bootstrap: name %q requested as both %s and %s", e.name, kindLabel(k), kindLabel(e.kind))
		}
		seen[e.name] = e.kind
	}
	supplied := getenv("PROTODEX_BOOTSTRAP_TOKEN")
	secret := supplied
	if secret == "" {
		var err error
		if secret, err = auth.GenerateSecret(); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
	} else if !auth.ValidSecretFormat(secret) {
		return errors.New("bootstrap: PROTODEX_BOOTSTRAP_TOKEN has an invalid format")
	}

	s, err := sqlite.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	// Owners, user and token are created in one transaction: all or nothing.
	var created []string
	var userCreated bool
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
		userCreated, err = auth.BootstrapIn(ctx, q, *user, secret)
		return err
	})
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	for _, c := range created {
		if _, err := fmt.Fprintf(stdout, "bootstrap: created %s\n", c); err != nil {
			return err
		}
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
  create [-role R] user NAME...
                         create users (role viewer, member or admin; default
                         member), printing an initial token for each
  create org NAME...     create organizations
  create mirror NAME...  reserve upstream owner names (e.g. googleapis) for mirrors
  list                   list all owners
  rename OLD NEW         rename a user or organization (mirrors cannot be renamed)
  role USER ROLE         set a user's instance role: viewer (read-only), member or admin
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
	roleFlag := fs.String("role", "", "instance role of created users: viewer, member (default) or admin")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()
	if *roleFlag != "" && action != "create" {
		return fmt.Errorf("owner %s: -role only applies to create", action)
	}

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
		role := store.RoleMember
		if *roleFlag != "" {
			if kind != owner.User {
				return errors.New("owner create: -role only applies to users")
			}
			if role, err = owner.ParseRole(*roleFlag); err != nil {
				return fmt.Errorf("owner create: %w", err)
			}
		}
		run = func(st store.Store) error { return createOwners(ctx, st, kind, role, rest[1:], stdout) }
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
	case "role":
		if len(rest) != 2 {
			return errors.New("owner role: want USER and ROLE (viewer, member or admin)")
		}
		run = func(st store.Store) error {
			var o store.Owner
			err := st.WithTx(ctx, func(q store.Querier) (err error) {
				o, err = owner.SetRole(ctx, q, rest[0], rest[1])
				return err
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "user %q is now %s\n", o.Name, o.Role)
			return err
		}
	case "help", "-h", "-help", "--help":
		_, err := fmt.Fprint(stdout, ownerUsage)
		return err
	default:
		_, _ = fmt.Fprint(stderr, ownerUsage)
		return fmt.Errorf("owner: unknown action %q", action)
	}

	return withDB(ctx, "owner "+action, *dbPath, run)
}

// withDB opens and migrates the database at path and runs fn on it,
// prefixing errors with what.
func withDB(ctx context.Context, what, path string, fn func(store.Store) error) error {
	if path == "" {
		return fmt.Errorf("%s: -db or PROTODEX_DB_PATH is required", what)
	}
	st, err := sqlite.Open(path)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if err := fn(st); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// createOwners creates all names atomically. Users get an initial token,
// printed once.
func createOwners(ctx context.Context, st store.Store, kind owner.Kind, role string, names []string, stdout io.Writer) error {
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
			if role != o.Role {
				if err := q.SetOwnerRole(ctx, o.ID, role); err != nil {
					return err
				}
				if _, err := audit.Record(ctx, q, audit.Event{Type: audit.UserRoleChanged, Subject: o.Name,
					Data: map[string]any{"user_id": o.ID, "from": o.Role, "to": role}}); err != nil {
					return err
				}
			}
			if secrets[i], err = auth.GenerateSecret(); err != nil {
				return err
			}
			tok, err := q.CreateToken(ctx, store.Token{OwnerID: o.ID, Name: "initial", Hash: auth.HashSecret(secrets[i])})
			if err != nil {
				return err
			}
			if _, err := audit.Record(ctx, q, audit.Event{Type: audit.TokenCreated, Subject: o.Name,
				Data: map[string]any{"token_id": tok.ID, "token_name": tok.Name, "source": "cli"}}); err != nil {
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
			_, err = fmt.Fprintf(stdout, "created user %q (%s). Token (shown once, store it safely):\n%s\n", n, role, secrets[i])
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
	_, _ = fmt.Fprintln(tw, "NAME\tKIND\tROLE\tID\tCREATED")
	for _, o := range owners {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", o.Name, kindLabel(owner.KindOf(o)), cmp.Or(o.Role, "-"), o.ID, o.CreatedAt.Format(time.RFC3339))
	}
	return tw.Flush()
}

const contributorUsage = `usage: protodex contributor <action> [-db path] OWNER/MODULE [args]

actions:
  add OWNER/MODULE USER ROLE  grant USER read, write or admin on the module
                              (replaces an existing grant)
  remove OWNER/MODULE USER    revoke USER's grant
  list OWNER/MODULE           list the module's contributors
`

// contributorCmd implements `protodex contributor`.
func contributorCmd(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, contributorUsage)
		return errors.New("contributor: missing action")
	}
	action := args[0]
	fs := flag.NewFlagSet("contributor "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, contributorUsage); fs.PrintDefaults() }
	dbPath := fs.String("db", getenv("PROTODEX_DB_PATH"), "SQLite database path (env PROTODEX_DB_PATH)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()
	want := map[string]int{"add": 3, "remove": 2, "list": 1}
	switch n, ok := want[action]; {
	case action == "help" || action == "-h" || action == "-help" || action == "--help":
		_, err := fmt.Fprint(stdout, contributorUsage)
		return err
	case !ok:
		_, _ = fmt.Fprint(stderr, contributorUsage)
		return fmt.Errorf("contributor: unknown action %q", action)
	case len(rest) != n:
		_, _ = fmt.Fprint(stderr, contributorUsage)
		return fmt.Errorf("contributor %s: want %d arguments", action, n)
	}
	ownerName, moduleName, ok := strings.Cut(rest[0], "/")
	if !ok || ownerName == "" || moduleName == "" || strings.Contains(moduleName, "/") {
		return fmt.Errorf("contributor %s: want OWNER/MODULE, got %q", action, rest[0])
	}
	return withDB(ctx, "contributor "+action, *dbPath, func(st store.Store) error {
		switch action {
		case "add":
			if err := st.WithTx(ctx, func(q store.Querier) error {
				return module.Grant(ctx, q, ownerName, moduleName, rest[1], rest[2])
			}); err != nil {
				return err
			}
			_, err := fmt.Fprintf(stdout, "granted %s on %s to %q\n", rest[2], rest[0], rest[1])
			return err
		case "remove":
			if err := st.WithTx(ctx, func(q store.Querier) error {
				return module.Revoke(ctx, q, ownerName, moduleName, rest[1])
			}); err != nil {
				return err
			}
			_, err := fmt.Fprintf(stdout, "revoked %q on %s\n", rest[1], rest[0])
			return err
		}
		cs, err := module.Contributors(ctx, st, ownerName, moduleName)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "USER\tROLE")
		for _, c := range cs {
			_, _ = fmt.Fprintf(tw, "%s\t%s\n", c.User.Name, c.Role)
		}
		return tw.Flush()
	})
}
