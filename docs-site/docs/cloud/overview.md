# PHPRay Cloud

The fleet console. Every server (or WordPress plugin) with a
`server_key` ships its data outbound over HTTPS to
`https://app.phpray.dev` — no inbound ports, no SSH
([ingest protocol](ingest.md)). You get:

- **Fleet view** — all servers and sites in one dashboard: slowest
  sites, error spikes, N+1 trends, per-plugin breakdowns, cross-server
  comparison
- **Alerts** — error-rate and latency alerts in the console; e-mail and
  webhook delivery are planned
- **Remote control** — "profile this URL for 10 minutes" pushed to the
  extension without touching php.ini (`GET /v1/control`)
- **Retention** — 7 to 90 days of traces and aggregates, by plan
- **Privacy controls** — per-site host pseudonymisation, sample-rate
  overrides, kill switch per site

## Plans

| | Free | Solo | Studio | Agency | Fleet | Host Edition |
|---|---|---|---|---|---|---|
| Price | 0 USD | 19 USD/mo | 99 USD/mo | 249 USD/mo | 499 USD/mo | 2,500 USD/yr up to 10 servers, +200 USD/yr per extra server |
| Sites | 1 | 3 | 25 | 100 | unlimited | unlimited |
| Servers | any | any | any | any | any | up to 10 (+200 USD/yr each) |
| Traces/day | 10,000 | 50,000 | 300,000 | 1,000,000 | 5,000,000 | per your hardware |
| History (traces and aggregates) | 7 days | 30 days | 30 days | 60 days | 90 days | your choice |
| Fleet view, per-site overview, slow queries, errors | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Before / after compare | — | — | ✓ | ✓ | ✓ | ✓ |
| Remote "profile this URL" from the console | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Alerts (console view) | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| E-mail / webhook alert delivery | planned | planned | planned | planned | planned | planned |
| White-label PDF reports, API | — | — | — | planned | planned | ✓ (self-hosted) |
| Self-hosted console + DirectAdmin admin plugin | — | — | — | — | — | ✓ |

Limits are enforced at ingest: a site above the plan's quota is refused, and traces
beyond the daily quota are dropped (aggregates keep flowing). Billing is monthly;
there is no overage pricing. Accounts: create a Free account on the
[pricing page](https://phpray.dev/#/pricing) (the console token is shown once);
paid plans are bought there by card (Stripe) or, while online payment is not
yet switched on, by e-mail to hello@phpray.dev. Server keys are created in the
console under *Servers → Add server* ([how to connect](https://phpray.dev/#/cloud)).

All plans include the local dashboard and full trace history on the box
itself; the console adds cross-server views and longer retention.
