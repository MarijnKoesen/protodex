// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
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
	steps := [][]string{
		{"init", "-q"},
		{"fetch", "-q", "--depth", "1", "--no-tags", "--", url, ref},
		{"checkout", "-q", "--detach", "FETCH_HEAD"},
	}
	for _, s := range steps {
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
