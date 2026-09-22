# phpray-mcp

An MCP server that lets an AI coding agent ask PHPRay what is slow.

It speaks the Model Context Protocol over stdio and reads the PHPRay Cloud
console API with **your own console token**, so an agent sees exactly the sites
that token is scoped to — no more, no less.

## Install

```bash
curl -fsSL -o phpray-mcp "https://phpray.dev/dl/$(curl -fsS https://phpray.dev/dl/LATEST)/phpray-mcp-linux-amd64"
chmod +x phpray-mcp && sudo mv phpray-mcp /usr/local/bin/
```

Builds are published for `linux-amd64`, `linux-arm64`, `darwin-amd64` and
`darwin-arm64`, with checksums in `SHA256SUMS` next to them.

## Two modes

**Cloud** — every server you own in one place, scoped to your console token.
The token is the 32-character string the console shows after you sign in, the
same one the sign-in page asks for:

```bash
claude mcp add phpray --env PHPRAY_TOKEN=<console token> -- phpray-mcp
```

**Local** — the collector on this machine, no account and no network:

```bash
claude mcp add phpray-local -- phpray-mcp
```

Local mode needs nothing when the collector has no `[auth] secret`. If it has
one, pass it as `PHPRAY_COLLECTOR_SECRET`; the server signs a short-lived token
per request, the same way the DirectAdmin plugin does.

Local mode adds two tools the cloud does not have: `phpray_diagnose`, which
returns the collector's own plain-language read of what is wrong with a domain,
and `phpray_php_versions`.

## Docker

The server speaks over stdio, so the image is for clients that launch
`docker run -i` and for directories that build it to check the server starts.

```bash
docker build -t phpray-mcp src/mcp
docker run -i --rm -e PHPRAY_TOKEN=<console token> phpray-mcp
```

With no environment at all it still starts and lists its tools; only calling a
tool needs a collector or a token.

## Use it with Claude Code

```bash
claude mcp add phpray --env PHPRAY_TOKEN=<console token> -- phpray-mcp
```

Environment:

| Variable | Meaning |
|---|---|
| `PHPRAY_URL` | console base URL, default `https://app.phpray.dev` |
| `PHPRAY_TOKEN` | your console token (Settings → Console token) |

## What the agent can ask

| Tool | Answers |
|---|---|
| `phpray_sites` | which sites report in, with requests, p95 and error rate |
| `phpray_overview` | one site's numbers for a time window |
| `phpray_slow_pages` | the slowest URLs, with request counts |
| `phpray_slow_queries` | SQL fingerprints by time spent, with callers |
| `phpray_components` | which plugin, theme or vendor package spent the time |
| `phpray_errors` | recent PHP errors with file and line |
| `phpray_traces` | recent requests, filterable by minimum duration and status |
| `phpray_trace` | one request in full: timing, queries, HTTP calls, components |
| `phpray_compare` | two time ranges side by side, to check a deployment |
| `phpray_alerts` | what is currently firing |
| `phpray_profile_url` | turn on per-function profiling for one URL prefix for N minutes |

Everything except `phpray_profile_url` is read-only.
