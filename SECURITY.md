# Security policy

## Reporting a vulnerability

Write to **security@phpray.dev**. Please include what you found, how to
reproduce it, and what an attacker could do with it. If you prefer, send an
encrypted message and ask for a key first.

We answer within **3 working days** and aim to ship a fix or a mitigation within
**14 days** for anything that lets one account read another's data, escalate
privileges, or crash a PHP worker. You will be credited in the changelog unless
you ask otherwise.

Please do not open a public issue for a vulnerability, and please do not test
against servers you do not own.

## What we consider a vulnerability

PHPRay runs **inside PHP**, often on shared hosting, so the bar is high:

- one account reading another account's traces, ring buffer or collector data,
- the extension crashing, hanging or corrupting memory in a PHP worker,
- the collector or its API exposing trace contents without authentication,
- a server key or console token being recoverable from data we store or log,
- anything that lets a site being traced influence the host through us.

Not vulnerabilities on their own: the collector API listening on localhost
without auth (that is the documented default; bind it elsewhere and it requires
a secret), or a site's own SQL appearing in its own traces.

## Supported versions

The current minor release gets security fixes. Older minors do not; upgrading a
PHP extension is one file and a worker recycle.

| Version | Supported |
|---|---|
| 0.15.x | yes |
| 0.14.x and older | no |

## Hardening notes

- The extension writes only to the paths in its configuration. On shared
  hosting the ring buffer is one file per uid with mode 0600.
- `phpray.master_switch=0` makes the extension inert at module startup: no
  observer, no handler replacement, no shared memory. Use it as the server-wide
  default and enable the extension per account.
- Query parameters and SQL values are masked before a record is written. Review
  `phpray.ignore_uris` for endpoints that carry secrets in the path.
