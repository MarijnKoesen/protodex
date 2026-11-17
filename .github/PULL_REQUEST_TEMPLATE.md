## What and why

<!-- Short description. Link issues with "Closes #123". -->

## How was this tested?

<!-- Commands run, new tests added. -->

## Checklist

- [ ] PR title follows Conventional Commits (`feat: …`, `fix: …`).
- [ ] All commits are signed off (`git commit -s`).
- [ ] New source files have an `SPDX-License-Identifier: Apache-2.0` header.
- [ ] `make lint test build` passes locally.
- [ ] **Clean-room:** this change uses only allowed public inputs
      (registry-proto, `buf` client source, public docs, black-box CLI
      behaviour) as described in CONTRIBUTING.md. It contains no code or
      fixtures from non-public/leaked Buf server code, decompiled images or
      closed-source protos.
