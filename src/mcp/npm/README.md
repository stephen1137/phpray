# phpray-mcp

MCP server for [PHPRay](https://phpray.dev): let an agent read what every PHP
request in production actually did — wall time, SQL fingerprints, outbound HTTP,
errors and N+1 patterns.

```bash
claude mcp add phpray --env PHPRAY_TOKEN=<console token> -- npx -y phpray-mcp
```

Or, on the server itself, with no account and no network:

```bash
PHPRAY_MODE=local npx -y phpray-mcp
```

Ask it, in your own words: *which of my sites is slowest right now*, *show the
N+1 requests from the last 24 hours*, *diagnose the request that took 79 seconds*.

It is read-only apart from one tool that turns profiling on for a URL prefix, and
in cloud mode it is scoped to a single console token, so the agent sees exactly
what that user sees.

## What this package is

A thin wrapper. The server itself is one static Go binary published with the
PHPRay release; `npm install` downloads the build for your platform from
`https://phpray.dev/dl/v<version>/` and **verifies its SHA-256** against the
checksums published next to it. The package version pins the release, so two
installs of the same version give you the same bytes.

Platforms: linux and macOS, amd64 and arm64. On anything else the install step
says so and exits without failing your project; build from source instead —
[src/mcp](https://github.com/stephen1137/phpray/tree/main/src/mcp).

Apache-2.0.
