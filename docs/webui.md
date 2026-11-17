# Web UI

`protodex serve` serves a small single-page app at `/` (assets under `/ui/`).
It is plain ES modules with no build step, embedded in the binary
(`internal/webui`), and talks only to the public Connect API using the Connect
JSON protocol — there are no private endpoints.

Pages: module list (filter by owner), module overview, commit history, labels,
a proto/file browser (by label or commit), access-token management (list,
create, delete; the secret is shown once) and sign-in.

- **Sign-in**: paste a personal access token (kept in `sessionStorage`, i.e.
  for the tab only). With OIDC enabled, "sign in with OIDC" (`/auth/login`)
  issues a token to paste. A UI-native OIDC session is not implemented.
- **Mirror provenance**: a commit's `source_control_url` is shown as "source"
  on the overview and file pages.
- **Accessibility / theme**: skip link, landmarks, labelled controls, focus
  outlines, `prefers-color-scheme` with a manual toggle.
- All API data is rendered with `textContent`; a strict CSP (no inline script)
  is set.

## End-to-end tests

Playwright tests live in `e2e/` and run in CI (`e2e` job):

```sh
make build
cd e2e && npm ci && npx playwright install chromium
E2E_TOKEN="pdx_$(head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n')" npx playwright test
```
