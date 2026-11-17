# Branch protection for `main`

Policy:

- No direct pushes; all changes land via pull request.
- At least 1 approving review, including a CODEOWNER; stale approvals are
  dismissed on new commits.
- Branch must be up to date and all required checks must pass:
  `build-test`, `lint`, `generated`, `licenses`, `proto`, `docker`, `helm`,
  `security`, `dependency-review`, `dco`, `pr-title`
  (described in [CONTRIBUTING.md](../CONTRIBUTING.md#required-status-checks)).
- No force pushes or branch deletion. Administrators are included.
- Squash merge only, so the Conventional Commits PR title becomes the commit
  message.

Notes:

- `dependency-review` needs the dependency graph enabled (on by default for
  public repositories; private repositories need GitHub Advanced Security).
- `gitleaks-action` needs a `GITLEAKS_LICENSE` secret only if the repository
  belongs to an organization. Personal-account repositories do not.
- A check only becomes selectable as "required" after it has run once, so
  apply this after the first CI run on a PR.

## Applying it

A repository admin can apply the policy with the GitHub CLI:

```sh
gh api -X PUT repos/marijnkoesen/wireyard/branches/main/protection \
  -H "Accept: application/vnd.github+json" --input - <<'JSON'
{
  "required_status_checks": {
    "strict": true,
    "checks": [
      {"context": "build-test"}, {"context": "lint"}, {"context": "generated"},
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

gh api -X PATCH repos/marijnkoesen/wireyard \
  -f allow_squash_merge=true -f allow_merge_commit=false -f allow_rebase_merge=false
```
