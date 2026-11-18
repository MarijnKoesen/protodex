// SPDX-License-Identifier: Apache-2.0

// Package migratebuf helps repositories move from the public Buf Schema
// Registry (or any other registry host) to a Protodex instance. It rewrites
// registry hosts in buf.yaml, reports remote plugins in buf.gen*.yaml that
// need a local replacement, and (optionally) re-runs `buf dep update`.
//
// Files are edited line by line so comments and formatting are preserved.
package migratebuf

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Options configures a run.
type Options struct {
	// Root is the directory to scan (recursively).
	Root string
	// From is the registry host to replace, e.g. "buf.build".
	From string
	// To is the Protodex host (and optional port) to use instead.
	To string
	// Apply writes changes and runs DepUpdate; otherwise nothing is modified.
	Apply bool
	// DepUpdate runs `buf dep update` in dir. Nil skips the step.
	DepUpdate func(ctx context.Context, dir string) error
}

// Rewrite is one changed line.
type Rewrite struct {
	File string // relative to Root
	Line int
	Old  string
	New  string
}

// RemotePlugin is a remote plugin reference found in a buf.gen*.yaml.
type RemotePlugin struct {
	File   string
	Line   int
	Ref    string // e.g. buf.build/protocolbuffers/go
	Local  string // suggested local plugin binary, "" if unknown
	Module string // suggested install hint, "" if unknown
}

// Result is the outcome of a run.
type Result struct {
	Rewrites []Rewrite
	Plugins  []RemotePlugin
	// UpdateDirs are the directories where `buf dep update` ran (Apply) or
	// would run (dry run).
	UpdateDirs []string
	// Updated is true when `buf dep update` actually ran.
	Updated bool
}

// pluginMap maps remote plugin paths (without host) to local equivalents.
var pluginMap = map[string][2]string{
	"protocolbuffers/go":          {"protoc-gen-go", "go tool directive: google.golang.org/protobuf/cmd/protoc-gen-go"},
	"grpc/go":                     {"protoc-gen-go-grpc", "go tool directive: google.golang.org/grpc/cmd/protoc-gen-go-grpc"},
	"connectrpc/go":               {"protoc-gen-connect-go", "go tool directive: connectrpc.com/connect/cmd/protoc-gen-connect-go"},
	"community/mfridman-go-json":  {"protoc-gen-go-json", "go tool directive: github.com/mfridman/protoc-gen-go-json"},
	"grpc-ecosystem/gateway":      {"protoc-gen-grpc-gateway", "go tool directive: github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway"},
	"bufbuild/es":                 {"protoc-gen-es", "npm: @bufbuild/protoc-gen-es"},
	"connectrpc/es":               {"protoc-gen-connect-es", "npm: @connectrpc/protoc-gen-connect-es"},
	"community/stephenh-ts-proto": {"protoc-gen-ts_proto", "npm: ts-proto"},
}

var (
	pluginLine = regexp.MustCompile(`^\s*-?\s*(?:remote|plugin):\s*["']?([^\s"'#]+)`)
	// pluginRef: host/owner/plugin[:version]
	pluginRef = regexp.MustCompile(`^([^/\s]+\.[^/\s]+|localhost(?::\d+)?)/([^/\s]+)/([^/:\s]+)`)
)

func isBufYAML(name string) bool { return name == "buf.yaml" }

func isBufGen(name string) bool {
	return name == "buf.gen.yaml" || (strings.HasPrefix(name, "buf.gen.") && strings.HasSuffix(name, ".yaml"))
}

// Run scans opts.Root and returns what was (or would be) changed.
func Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.From == "" || opts.To == "" {
		return nil, errors.New("both a source and a target host are required")
	}
	if strings.Contains(opts.From, "/") || strings.Contains(opts.To, "/") {
		return nil, errors.New("hosts must not contain a scheme or path (use host[:port])")
	}
	if opts.From == opts.To {
		return nil, errors.New("source and target hosts are identical")
	}
	res := &Result{}
	updateDirs := map[string]bool{}

	err := filepath.WalkDir(opts.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				if path != opts.Root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, _ := filepath.Rel(opts.Root, path)
		switch {
		case isBufYAML(d.Name()):
			n, err := rewriteFile(path, rel, opts, res)
			if err != nil {
				return err
			}
			if n > 0 {
				updateDirs[filepath.Dir(path)] = true
			}
		case isBufGen(d.Name()):
			return scanPlugins(path, rel, opts.From, res)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for d := range updateDirs {
		res.UpdateDirs = append(res.UpdateDirs, d)
	}
	sort.Strings(res.UpdateDirs)

	if opts.Apply && opts.DepUpdate != nil {
		for _, d := range res.UpdateDirs {
			if err := opts.DepUpdate(ctx, d); err != nil {
				return res, fmt.Errorf("buf dep update in %s: %w", d, err)
			}
		}
		res.Updated = true
	}
	return res, nil
}

// rewriteFile replaces `from/` with `to/` at the start of module references:
// list items (deps) and `name:` values. It returns the number of changed lines.
func rewriteFile(path, rel string, opts Options, res *Result) (int, error) {
	data, err := os.ReadFile(path) //nolint:gosec // paths come from walking the directory the operator named
	if err != nil {
		return 0, err
	}
	re := regexp.MustCompile(`^(\s*(?:-\s*|name:\s*)["']?)` + regexp.QuoteMeta(opts.From) + `/`)
	lines := strings.Split(string(data), "\n")
	n := 0
	for i, l := range lines {
		if !re.MatchString(l) {
			continue
		}
		nl := re.ReplaceAllString(l, "${1}"+opts.To+"/")
		res.Rewrites = append(res.Rewrites, Rewrite{File: rel, Line: i + 1, Old: strings.TrimSpace(l), New: strings.TrimSpace(nl)})
		lines[i] = nl
		n++
	}
	if n > 0 && opts.Apply {
		info, err := os.Stat(path)
		if err != nil {
			return 0, err
		}
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), info.Mode().Perm()); err != nil { //nolint:gosec // rewriting the operator's own file in place
			return 0, err
		}
	}
	return n, nil
}

func scanPlugins(path, rel, from string, res *Result) error {
	data, err := os.ReadFile(path) //nolint:gosec // paths come from walking the directory the operator named
	if err != nil {
		return err
	}
	for i, l := range strings.Split(string(data), "\n") {
		m := pluginLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		ref := pluginRef.FindStringSubmatch(m[1])
		if ref == nil {
			continue // local plugin name
		}
		if from != "" && ref[1] != from {
			continue
		}
		p := RemotePlugin{File: rel, Line: i + 1, Ref: m[1]}
		if v, ok := pluginMap[ref[2]+"/"+ref[3]]; ok {
			p.Local, p.Module = v[0], v[1]
		}
		res.Plugins = append(res.Plugins, p)
	}
	return nil
}

// Report renders a human-readable summary.
func (r *Result) Report(apply bool) string {
	var b strings.Builder
	verb := "would rewrite"
	if apply {
		verb = "rewrote"
	}
	if len(r.Rewrites) == 0 {
		b.WriteString("no registry hosts to rewrite\n")
	}
	for _, w := range r.Rewrites {
		fmt.Fprintf(&b, "%s %s:%d\n  - %s\n  + %s\n", verb, w.File, w.Line, w.Old, w.New)
	}
	if len(r.Plugins) > 0 {
		b.WriteString("\nremote plugins (Protodex does not host plugins; switch to local ones):\n")
		for _, p := range r.Plugins {
			if p.Local != "" {
				fmt.Fprintf(&b, "  %s:%d  %s -> local: %s (%s)\n", p.File, p.Line, p.Ref, p.Local, p.Module)
			} else {
				fmt.Fprintf(&b, "  %s:%d  %s -> no known local equivalent\n", p.File, p.Line, p.Ref)
			}
		}
	}
	for _, d := range r.UpdateDirs {
		if r.Updated {
			fmt.Fprintf(&b, "\nran buf dep update in %s\n", d)
		} else {
			fmt.Fprintf(&b, "\nwould run buf dep update in %s\n", d)
		}
	}
	if !apply {
		b.WriteString("\ndry run: nothing was changed; re-run with -apply to write changes\n")
	}
	return b.String()
}
