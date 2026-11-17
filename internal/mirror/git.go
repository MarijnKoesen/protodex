// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Snapshot is a checked-out upstream tree.
type Snapshot struct {
	// Commit is the full upstream commit hash the ref resolved to.
	Commit string
	// FS is the tree at that commit.
	FS fs.FS
	// Close releases the snapshot.
	Close func()
}

// Fetcher obtains the tree of an upstream repository at a pinned ref.
type Fetcher interface {
	Fetch(ctx context.Context, url, ref string) (Snapshot, error)
}

// GitFetcher fetches with the git command line, shallowly, over https (and
// ssh). Set AllowFile to also accept file:// URLs (tests, local mirrors).
type GitFetcher struct {
	AllowFile bool
}

// Fetch implements Fetcher.
func (g GitFetcher) Fetch(ctx context.Context, url, ref string) (Snapshot, error) {
	dir, err := os.MkdirTemp("", "protodex-mirror-*")
	if err != nil {
		return Snapshot{}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	protocols := "https:ssh"
	if g.AllowFile {
		protocols += ":file"
	}
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext( //nolint:gosec // url and ref are validated by Config; passed after "--"
			ctx, "git", append([]string{"-C", dir, "-c", "core.symlinks=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL="+protocols,
			"GIT_CONFIG_NOSYSTEM=1")
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
		}
		return strings.TrimSpace(out.String()), nil
	}
	// A tag is fetched as refs/tags/<ref> so a branch of the same name (or
	// main/HEAD) can never satisfy the pin; commit hashes are fetched as is.
	fetchRef := ref
	if !commitRE.MatchString(ref) {
		fetchRef = "refs/tags/" + ref
	}
	steps := [][]string{
		{"init", "-q"},
		{"fetch", "-q", "--depth", "1", "--no-tags", "--", url, fetchRef},
	}
	// Check out the committed bytes: .gitattributes (eol, filter, ident) must
	// not transform what is mirrored; info/attributes overrides the tree's.
	const attrs = "* -text -eol -filter -ident -export-subst\n"
	steps = append(steps, []string{"-c", "core.autocrlf=false", "checkout", "-q", "--detach", "FETCH_HEAD"})
	for i, s := range steps {
		if i == 2 { // after init and fetch: write attributes before checkout
			if err := os.WriteFile(filepath.Join(dir, ".git", "info", "attributes"), []byte(attrs), 0o600); err != nil {
				cleanup()
				return Snapshot{}, err
			}
		}
		if _, err := run(s...); err != nil {
			cleanup()
			return Snapshot{}, err
		}
	}
	commit, err := run("rev-parse", "HEAD")
	if err != nil {
		cleanup()
		return Snapshot{}, err
	}
	return Snapshot{Commit: commit, FS: os.DirFS(dir), Close: cleanup}, nil
}
