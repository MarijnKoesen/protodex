# Branch protection for `main`

Policy:

- No direct pushes; all changes land via pull request.
- At least 1 approving review, including a CODEOWNER; stale approvals are
  dismissed on new commits.
- Branch must be up to date and all required checks must pass:
  `build-test`, `docs`, `lint`, `generated`, `licenses`, `proto`, `docker`, `helm`,
  `security`, `dependency-review`, `dco`, `pr-title`
  (described in [CONTRIBUTING.md](https://github.com/MarijnKoesen/protodex/blob/main/CONTRIBUTING.md#required-status-checks)).
- No force pushes or branch deletion. Administrators are included.
- Squash merge only, so the Conventional Commits PR title becomes the commit
  message.

Notes:

- `dependency-review` runs on public repositories. For private repositories,
  enable the dependency graph and GitHub Advanced Security, then set the
  `DEPENDENCY_REVIEW_ENABLED` Actions variable to `true`; otherwise the job is
  skipped. The `licenses` and `security` checks still run.
- `gitleaks-action` needs a `GITLEAKS_LICENSE` secret only if the repository
  belongs to an organization. Personal-account repositories do not.
- A check only becomes selectable as "required" after it has run once, so
  apply this after the first CI run on a PR.
- `kind-e2e` (Helm chart e2e on kind, [deploy/e2e](https://github.com/MarijnKoesen/protodex/blob/main/deploy/e2e/README.md))
  is not in the list yet. Add it once it has proven stable; see below.

## kind-e2e and path filters

The kind e2e takes several minutes, so it only does real work when a PR
touches `deploy/`, `internal/`, `cmd/`, `test/conformance/`, `go.mod`/`go.sum`,
the `Dockerfile`, the `Makefile` or its workflow. That filter must **not** be
a workflow-level `on.pull_request.paths` filter: a workflow skipped that way
reports no status at all, and a required check that never reports leaves the
PR "Expected — waiting for status" forever.

Instead `.github/workflows/kind-e2e.yml` runs on every PR. The `kind-e2e` job
always starts, computes the changed files against the PR base itself and
skips its remaining steps when none match, so the check still reports
success. If the change detection itself fails, the job fails (it is a step
inside the job, not a separate job whose failure would make `kind-e2e`
"skipped", which GitHub counts as passing).

To make it required, add `{"context": "kind-e2e"}` to the `checks` list
below. When changing the path list, keep it in the workflow's `changes` step
(not in `on:`), and keep this section and
[deploy/e2e/README.md](https://github.com/MarijnKoesen/protodex/blob/main/deploy/e2e/README.md#ci) in sync.

## Applying it

A repository admin can apply the policy with the GitHub CLI:

```sh
gh api -X PUT repos/marijnkoesen/protodex/branches/main/protection \
  -H "Accept: application/vnd.github+json" --input - <<'JSON'
{
  "required_status_checks": {
    "strict": true,
    "checks": [
      {"context": "build-test"}, {"context": "docs"}, {"context": "lint"}, {"context": "generated"},
      {"context": "licenses"}, {"context": "proto"}, {"context": "docker"},
      {"context": "helm"}, {"context": "security"},
      {"context": "dependency-review"}, {"context": "dco"},
      {"context": "pr-title"}
    ]
  },
  "enforce_admins": true,
  "required_pull_request_reviews": {
    "required_approving_review_count": 1,
    "require_code_owner_reviews": true,
    "dismiss_stale_reviews": true
  },
  "restrictions": null,
  "allow_force_pushes": false,
  "allow_deletions": false,
  "required_conversation_resolution": true
}
JSON

gh api -X PATCH repos/marijnkoesen/protodex \
  -f allow_squash_merge=true -f allow_merge_commit=false -f allow_rebase_merge=false
```
