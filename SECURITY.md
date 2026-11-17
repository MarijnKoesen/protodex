# Security Policy

## Reporting a vulnerability

**Do not open a public issue for security problems.**

Report vulnerabilities privately through GitHub's
[private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
("Report a vulnerability" under this repository's **Security** tab).

Please include affected versions/commits, reproduction steps and impact.
We aim to acknowledge reports within 3 business days and to agree on a
disclosure timeline with you.

## Supported versions

Protodex has no releases yet. Until 1.0, only the latest commit on `main`
receives security fixes.

## Disclosure process

1. Report privately (above). Do not disclose publicly yet.
2. We acknowledge within 3 business days and confirm or dispute the issue.
3. We develop a fix privately and agree on a disclosure date with you
   (target: within 90 days of the report).
4. We release the fix, publish a GitHub security advisory and credit you
   unless you prefer otherwise.

## Hardening documentation

- [Threat model](docs/threat-model.md)
- [Security review checklist](docs/security-checklist.md)
- Rate limits and push quotas: see `rate_limit` and `max_push_*` in the
  [deployment guide](deploy/README.md).
