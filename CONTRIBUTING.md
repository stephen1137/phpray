# Contributing to PHPRay

Contributions are welcome under the Apache-2.0 license. We use the
Developer Certificate of Origin (https://developercertificate.org): add
`Signed-off-by: Your Name <you@example.com>` to every commit (`git commit -s`).
No CLA.

- Bugs and feature requests: GitHub issues.
- Security issues: see SECURITY.md (do not open a public issue).
- Overhead is a product requirement: any change to the extension hot path
  (RINIT/RSHUTDOWN, hooks, observer) must come with before/after numbers from
  `docker/benchmark.sh` on a real WordPress/WooCommerce site.
