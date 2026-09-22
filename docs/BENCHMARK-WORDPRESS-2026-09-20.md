# Overhead on WordPress — measured 2026-09-20

This is the measurement behind every performance claim we make. It replaces the
earlier informal numbers taken on a busy production server.

## What was measured

A WordPress page render, end to end over HTTP, with and without the extension.

| | |
|---|---|
| Application | WordPress 7.1, 31 published posts, default theme, 89 KB HTML |
| PHP | 8.3.33, mod_php under Apache, opcache on |
| Database | MariaDB 11 in a separate container |
| Host | 16-core laptop on mains power, `powersave` governor |
| Isolation | web container pinned to CPUs 4–7, database to 2–3, load generator to 12–15 |
| Client | `ab -n 300 -c 1` (sequential, so this is latency, not throughput) |
| Warm-up | 120 requests after every restart, discarded |
| Design | variants interleaved, 5 rounds, so machine drift is spread evenly |

Interleaving matters: measuring one variant to completion and then the next
attributes thermal drift and background load to whichever ran later.

## Results

Median across 5 rounds of the per-round median request time.

| Variant | p50 | p95 | vs baseline |
|---|---|---|---|
| No extension | 48 ms | 51 ms | — |
| Loaded, `master_switch=0` (inert) | 46 ms | 48 ms | no measurable difference |
| Enabled, function observer **off** | 47 ms | 53 ms | no measurable difference |
| Enabled, function observer **on** (current default) | 50 ms | 53 ms | **+2 ms, about +4 %** |

Per-round p50 values, to show the spread:

| Variant | rounds |
|---|---|
| No extension | 46, 47, 48, 48, 50 |
| Observer off | 46, 47, 47, 48, 48 |
| Observer on | 49, 49, 50, 51, 52 |

The first two distributions overlap almost completely. The third is separated
from both.

## What this says

1. **The always-on tracing layer is not what costs.** With the function observer
   off, we could not separate the extension from the baseline across 5 rounds.
2. **The function observer is the whole cost.** It is registered at module start
   whenever `phpray.profile_functions=1`, which is today's default, so every
   request pays for it even though only 3 % of requests are actually profiled.
3. **The output path does not matter.** A separate 5-round run compared writing
   to a file against the shared-memory ring: 50 ms against 50 ms. Records are
   small and written once per request.
4. **`mode=all` costs the same as `mode=smart`.** Smart mode instruments the
   request either way and only decides what to keep at the end.

## The observer costs the same whether or not it ever fires

A follow-up run, 3 rounds, added a variant with the observer registered but
sampling set to zero, so no request is ever profiled:

| Variant | p50 |
|---|---|
| No extension | 47 ms |
| Observer registered, sampling 0 % (never profiles) | 50 ms |
| Observer registered, sampling 3 % (default) | 50 ms |

The two observed variants are the same. The cost is not our handlers, which
return immediately when the request is not profiled, and it is not the sampling
rate. It is PHP itself: once any extension registers a function observer, the
engine switches every user function call onto the observed path for the life of
the process.

That makes the trade-off binary rather than tunable. `phpray.profile_functions`
is either on, and every request pays about 4 % on this workload, or off, and
the component breakdown is unavailable in that process. Lowering the sample
rate saves nothing. This is worth saying out loud in the documentation, because
the intuitive reading of "sampled at 3 %" is that the cost is also 3 %.

## What we will not claim

- That the overhead is zero. With the default configuration it is not.
- That these numbers transfer to a different application. This is one WordPress
  page on one machine. A request dominated by database or external HTTP time
  will show a smaller relative cost; a tight PHP loop will show more.
- Anything about throughput. The client was sequential on purpose.

## Next engineering step

The registration callback already declines core and vendor code and only
observes files it can attribute to a plugin or theme, so the obvious
optimisation is already in place. What remains is the engine-wide switch
described above, which no extension can avoid while an observer is registered.

Two directions are worth trying, in this order:

1. **Decide per worker, not per request.** A host could arm the observer in a
   small share of its PHP workers and leave the rest untouched, so the fleet
   pays a few percent on a few percent of traffic instead of a few percent on
   all of it. This needs the collector to know which workers are armed.
2. **Measure the same thing without the observer.** Attribution of time to a
   plugin can be approximated from the SQL, HTTP and file hooks we already
   install, which cost nothing measurable. It is coarser than per-function
   timing but answers the same customer question.

Until one of them lands, the honest guidance is: leave
`phpray.profile_functions=0` on latency-sensitive fleets and arm it when
investigating.

## Reproducing

The harness is in `tools/bench-wordpress/`. It builds the extension for the
container's PHP, installs WordPress, and runs the interleaved rounds.
