# Security

## Reporting a vulnerability

Email: <icassimail@proton.me>

Please include: description, steps to reproduce, impact, optional handle
for credit.

We aim to acknowledge within 48 hours and patch within 7 days for
critical issues, 30 days for low-severity.

## Scope

- **Auth bypass** — the `auth: tailscale` check is critical. If you can
  bypass it from outside the tailnet, that's a critical issue.
- **Path traversal** — we open files in the data dir; ensure `..` is
  blocked everywhere.
- **Cert handling** — Wave 2 will introduce Let's Encrypt. If you can
  trick the daemon into issuing a cert for someone else's domain, that's
  a critical issue.
- **MCP/ACP injection** — both endpoints are unauthenticated. If your
  agent can be tricked into calling `aigoproxy_remove` on a host it
  shouldn't, that's a problem.

## Out of scope

- DDoS — aigoproxy is behind Tailscale, so the attack surface is small
- Multi-tenant isolation — single-user personal project
