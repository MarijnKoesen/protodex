// SPDX-License-Identifier: Apache-2.0

// Package trust implements CI trust credentials: a CI workload (a GitHub
// Actions job, a Kubernetes pod) presents an identity token signed by its
// platform and, if the token's claims satisfy a credential's rules, gets a
// short-lived registry token restricted to one module. No long-lived
// registry secret has to be stored in CI. See docs/ci-oidc.md.
package trust

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/marijnkoesen/protodex/internal/config"
	"github.com/marijnkoesen/protodex/internal/store"
)

// Spec describes a trust credential to create, by names rather than IDs.
type Spec struct {
	// Name identifies the credential (unique).
	Name string
	// Module is "OWNER/MODULE", the only module minted tokens can access.
	Module string
	// User is the user minted tokens act as. It needs a contributor grant
	// (or namespace role) on Module: the token never has more than that.
	User string
	// Provider is store.TrustGitHub (default) or store.TrustKubernetes.
	Provider string
	// Issuer defaults to config.GitHubActionsIssuer for GitHub and is
	// required for Kubernetes. The server must also trust it in its
	// ci_trust configuration.
	Issuer string
	// Audience the identity token must be issued for (required).
	Audience string
	// Subject, Repository and Ref are the claim rules (see Credential).
	Subject    string
	Repository string
	Ref        string
	// Role is store.ContributorRead or store.ContributorWrite (default).
	Role string
	// TTL of minted tokens; zero uses the server default.
	TTL time.Duration
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// Validate checks a credential's rules. Rules fail closed: a GitHub
// credential must pin the repository, a Kubernetes credential the subject
// (service account), so that a credential never trusts a whole issuer.
func Validate(c store.TrustCredential) error {
	if !nameRE.MatchString(c.Name) {
		return fmt.Errorf("invalid credential name %q: use letters, digits, '.', '_' or '-' (at most 100)", c.Name)
	}
	var errs []error
	if c.Issuer == "" {
		errs = append(errs, errors.New("issuer is required"))
	}
	if strings.TrimSpace(c.Audience) == "" {
		errs = append(errs, errors.New("audience is required"))
	}
	switch c.Role {
	case store.ContributorRead, store.ContributorWrite:
	default:
		errs = append(errs, fmt.Errorf("role %q: want read or write", c.Role))
	}
	if c.TokenTTL < 0 || c.TokenTTL > config.MaxCITokenTTL {
		errs = append(errs, fmt.Errorf("token TTL must be between 0 (server default) and %s", config.MaxCITokenTTL))
	}
	for _, p := range []struct{ what, pattern string }{{"subject", c.SubjectPattern}, {"ref", c.RefPattern}} {
		if p.pattern != "" && strings.Trim(p.pattern, "*?") == "" {
			errs = append(errs, fmt.Errorf("%s pattern %q matches anything: leave it empty or be specific", p.what, p.pattern))
		}
	}
	switch c.Provider {
	case store.TrustGitHub:
		if c.Repository == "" {
			errs = append(errs, errors.New("github credentials require a repository (OWNER/REPO)"))
		} else if o, r, ok := strings.Cut(c.Repository, "/"); !ok || o == "" || r == "" || strings.ContainsAny(r, "/*?[") || strings.ContainsAny(o, "*?[") {
			errs = append(errs, fmt.Errorf("repository %q: want OWNER/REPO (no wildcards)", c.Repository))
		}
	case store.TrustKubernetes:
		if c.SubjectPattern == "" {
			errs = append(errs, errors.New("kubernetes credentials require a subject (system:serviceaccount:NAMESPACE:NAME)"))
		}
		if c.Repository != "" || c.RefPattern != "" {
			errs = append(errs, errors.New("repository and ref rules only apply to github credentials"))
		}
	default:
		errs = append(errs, fmt.Errorf("provider %q: want github or kubernetes", c.Provider))
	}
	return errors.Join(errs...)
}

// Claims are the identity-token claims the rules look at.
type Claims struct {
	Subject    string `json:"sub"`
	Repository string `json:"repository"`
	Ref        string `json:"ref"`
}

// Match reports whether claims satisfy c's rules, and if not, which rule
// failed (for server-side logs only; never tell the client). In patterns
// '*' matches any run of characters (including '/' and ':') and '?' any
// single character; everything else matches literally. Every configured
// rule must match and an empty claim never matches.
func Match(c store.TrustCredential, cl Claims) (ok bool, failed string) {
	if c.Provider == store.TrustGitHub && c.Repository == "" ||
		c.Provider == store.TrustKubernetes && c.SubjectPattern == "" ||
		c.Provider != store.TrustGitHub && c.Provider != store.TrustKubernetes {
		return false, "credential has no mandatory rule"
	}
	if c.Repository != "" && (cl.Repository == "" || cl.Repository != c.Repository) {
		return false, "repository"
	}
	if c.SubjectPattern != "" && !glob(c.SubjectPattern, cl.Subject) {
		return false, "subject"
	}
	if c.RefPattern != "" && !glob(c.RefPattern, cl.Ref) {
		return false, "ref"
	}
	return true, ""
}

func glob(pattern, s string) bool {
	if s == "" {
		return false
	}
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	return err == nil && re.MatchString(s)
}

func splitModule(full string) (string, string, error) {
	o, m, ok := strings.Cut(full, "/")
	if !ok || o == "" || m == "" || strings.Contains(m, "/") {
		return "", "", fmt.Errorf("want OWNER/MODULE, got %q", full)
	}
	return o, m, nil
}

// Add creates the credential described by s.
func Add(ctx context.Context, q store.Querier, s Spec) (store.TrustCredential, error) {
	ownerName, moduleName, err := splitModule(s.Module)
	if err != nil {
		return store.TrustCredential{}, err
	}
	c := store.TrustCredential{Name: s.Name, Provider: s.Provider, Issuer: s.Issuer, Audience: s.Audience,
		SubjectPattern: s.Subject, Repository: s.Repository, RefPattern: s.Ref, Role: s.Role, TokenTTL: s.TTL}
	if c.Provider == "" {
		c.Provider = store.TrustGitHub
	}
	if c.Issuer == "" && c.Provider == store.TrustGitHub {
		c.Issuer = config.GitHubActionsIssuer
	}
	if c.Role == "" {
		c.Role = store.ContributorWrite
	}
	if err := Validate(c); err != nil {
		return store.TrustCredential{}, err
	}
	o, err := q.GetOwner(ctx, ownerName)
	if err != nil {
		return store.TrustCredential{}, fmt.Errorf("owner %q: %w", ownerName, err)
	}
	if o.Mirror {
		return store.TrustCredential{}, fmt.Errorf("module %s belongs to mirror owner %q and is read-only", s.Module, o.Name)
	}
	m, err := q.GetModule(ctx, o.ID, moduleName)
	if err != nil {
		return store.TrustCredential{}, fmt.Errorf("module %s: %w", s.Module, err)
	}
	u, err := q.GetOwner(ctx, s.User)
	if err != nil {
		return store.TrustCredential{}, fmt.Errorf("user %q: %w", s.User, err)
	}
	if u.Type != store.OwnerUser {
		return store.TrustCredential{}, fmt.Errorf("%q is not a user: tokens can only act as users", s.User)
	}
	c.ModuleID, c.UserID = m.ID, u.ID
	c, err = q.CreateTrustCredential(ctx, c)
	if errors.Is(err, store.ErrConflict) {
		return store.TrustCredential{}, fmt.Errorf("trust credential %q already exists", s.Name)
	}
	return c, err
}

// Listed is a credential with its module and user resolved to names.
type Listed struct {
	store.TrustCredential
	Module string // OWNER/MODULE
	User   string
}

// List returns all credentials, or only those of module "OWNER/MODULE"
// when module is not empty.
func List(ctx context.Context, q store.Querier, module string) ([]Listed, error) {
	cs, err := listCredentials(ctx, q, module)
	if err != nil {
		return nil, err
	}
	out := make([]Listed, 0, len(cs))
	for _, c := range cs {
		m, err := q.GetModuleByID(ctx, c.ModuleID)
		if err != nil {
			return nil, err
		}
		o, err := q.GetOwnerByID(ctx, m.OwnerID)
		if err != nil {
			return nil, err
		}
		u, err := q.GetOwnerByID(ctx, c.UserID)
		if err != nil {
			return nil, err
		}
		out = append(out, Listed{TrustCredential: c, Module: o.Name + "/" + m.Name, User: u.Name})
	}
	return out, nil
}

func listCredentials(ctx context.Context, q store.Querier, module string) ([]store.TrustCredential, error) {
	if module == "" {
		return q.ListTrustCredentials(ctx)
	}
	ownerName, moduleName, err := splitModule(module)
	if err != nil {
		return nil, err
	}
	o, err := q.GetOwner(ctx, ownerName)
	if err != nil {
		return nil, fmt.Errorf("owner %q: %w", ownerName, err)
	}
	m, err := q.GetModule(ctx, o.ID, moduleName)
	if err != nil {
		return nil, fmt.Errorf("module %s: %w", module, err)
	}
	return q.ListModuleTrustCredentials(ctx, m.ID)
}

// Remove deletes the named credential. Tokens it already minted stay valid
// until they expire (at most config.MaxCITokenTTL); revoke them through
// the user's tokens if needed.
func Remove(ctx context.Context, q store.Querier, name string) error {
	if err := q.DeleteTrustCredential(ctx, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("trust credential %q not found", name)
		}
		return err
	}
	return nil
}
