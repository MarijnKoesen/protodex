// SPDX-License-Identifier: Apache-2.0

// Package mirror imports modules from upstream git repositories (googleapis,
// protovalidate, grpc-gateway, ...) onto a mirror owner of this registry, at
// pinned tags or commits. File paths and contents are preserved exactly, so
// published modules can depend on the same <host>/googleapis/googleapis that
// their buf.yaml names upstream.
package mirror

import (
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/marijnkoesen/protodex/internal/module"
	"github.com/marijnkoesen/protodex/internal/owner"
)

// AllowedLicenses are the SPDX identifiers whose terms permit redistribution
// (with the license text, which every mirrored module carries as LICENSE).
// An upstream under any other license is refused until docs/mirror-licenses.md
// records a review and the identifier is added here.
var AllowedLicenses = []string{"Apache-2.0", "BSD-2-Clause", "BSD-3-Clause", "MIT"}

// Config is the content of mirror.yaml.
type Config struct {
	Upstreams []Upstream `yaml:"upstreams"`
}

// Upstream is one mirrored module.
type Upstream struct {
	// Name is "owner/module" on this registry, the same path as upstream
	// (e.g. googleapis/googleapis). The owner is a mirror owner, created on
	// demand.
	Name string `yaml:"name"`
	// URL is the git repository to import from.
	URL string `yaml:"url"`
	// Ref pins the import: a tag or a full commit hash. Branches are not
	// accepted because they move.
	Ref string `yaml:"ref"`
	// License is the SPDX identifier of the upstream's license.
	License string `yaml:"license"`
	// Roots are the directories of the repository holding proto roots. Files
	// are stored relative to their root. Default: the repository root.
	Roots []string `yaml:"roots"`
	// Include limits the stored .proto files to these path prefixes (relative
	// to the root). Default: all.
	Include []string `yaml:"include"`
	// Labels are extra labels pointed at the imported commit. The label
	// "main" (the default label, so unpinned dependencies resolve) and, for
	// a tag pin, the tag are always set.
	Labels []string `yaml:"labels"`
	// Deps are the names of other upstreams in this config that this module
	// depends on, imported first and recorded as commit dependencies.
	Deps []string `yaml:"deps"`
}

// Owner returns the owner part of the name.
func (u Upstream) Owner() string { o, _, _ := strings.Cut(u.Name, "/"); return o }

// Module returns the module part of the name.
func (u Upstream) Module() string { _, m, _ := strings.Cut(u.Name, "/"); return m }

var commitRE = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// PinnedToTag reports whether Ref is a tag rather than a commit hash.
func (u Upstream) PinnedToTag() bool { return !commitRE.MatchString(u.Ref) }

// LabelNames returns the labels the imported commit gets.
func (u Upstream) LabelNames() []string {
	names := []string{module.DefaultLabel}
	if u.PinnedToTag() {
		names = append(names, u.Ref)
	}
	for _, l := range u.Labels {
		if !slices.Contains(names, l) {
			names = append(names, l)
		}
	}
	return names
}

// LoadFile reads and validates a mirror config file.
func LoadFile(file string) (*Config, error) {
	data, err := os.ReadFile(file) //nolint:gosec // operator-supplied config path
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse parses and validates mirror.yaml content. Unknown fields are errors.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("mirror config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("mirror config: %w", err)
	}
	return &c, nil
}

// Validate checks the config.
func (c *Config) Validate() error {
	if len(c.Upstreams) == 0 {
		return errors.New("no upstreams")
	}
	seen := map[string]bool{}
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		if err := u.validate(); err != nil {
			return fmt.Errorf("upstream %q: %w", u.Name, err)
		}
		if seen[u.Name] {
			return fmt.Errorf("upstream %q appears twice", u.Name)
		}
		seen[u.Name] = true
	}
	for _, u := range c.Upstreams {
		for _, d := range u.Deps {
			if !seen[d] {
				return fmt.Errorf("upstream %q: dependency %q is not in the config", u.Name, d)
			}
			if d == u.Name {
				return fmt.Errorf("upstream %q depends on itself", u.Name)
			}
		}
	}
	if _, err := c.Order(); err != nil {
		return err
	}
	return nil
}

func (u *Upstream) validate() error {
	o, m, ok := strings.Cut(u.Name, "/")
	if !ok || strings.Contains(m, "/") {
		return errors.New(`name must be "owner/module"`)
	}
	if err := owner.ValidateName(owner.Mirror, o); err != nil {
		return err
	}
	if err := module.ValidateName(m); err != nil {
		return err
	}
	if !strings.Contains(u.URL, "://") || strings.HasPrefix(u.URL, "-") {
		return errors.New("url must be a git URL such as https://github.com/org/repo.git")
	}
	if u.Ref == "" || strings.HasPrefix(u.Ref, "-") || strings.ContainsAny(u.Ref, " \t\n:") {
		return errors.New("ref must be a tag or full commit hash")
	}
	if strings.HasPrefix(u.Ref, "refs/heads/") {
		return errors.New("ref must be a tag or commit, not a branch")
	}
	if !slices.Contains(AllowedLicenses, u.License) {
		return fmt.Errorf("license %q is not approved for redistribution (see docs/mirror-licenses.md); approved: %s",
			u.License, strings.Join(AllowedLicenses, ", "))
	}
	if len(u.Roots) == 0 {
		u.Roots = []string{"."}
	}
	for _, r := range append(slices.Clone(u.Roots), u.Include...) {
		if r == "" || path.IsAbs(r) || path.Clean(r) == ".." || strings.HasPrefix(path.Clean(r), "../") || strings.Contains(r, `\`) {
			return fmt.Errorf("path %q must be a relative path inside the repository", r)
		}
	}
	for _, l := range u.Labels {
		if err := module.ValidateLabelName(l); err != nil {
			return err
		}
	}
	return nil
}

// Order returns the upstreams so that every upstream follows its
// dependencies. A dependency cycle is an error.
func (c *Config) Order() ([]Upstream, error) {
	by := map[string]Upstream{}
	for _, u := range c.Upstreams {
		by[u.Name] = u
	}
	var out []Upstream
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(name string) error
	visit = func(name string) error {
		switch state[name] {
		case 2:
			return nil
		case 1:
			return fmt.Errorf("dependency cycle through %q", name)
		}
		state[name] = 1
		for _, d := range by[name].Deps {
			if err := visit(d); err != nil {
				return err
			}
		}
		state[name] = 2
		out = append(out, by[name])
		return nil
	}
	for _, u := range c.Upstreams {
		if err := visit(u.Name); err != nil {
			return nil, err
		}
	}
	return out, nil
}
