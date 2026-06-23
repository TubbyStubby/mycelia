# mycelia

Compare and analyze V8 CPU profiles produced by the Node.js auto-profiler.
Profiles are loaded from Google Cloud Storage (or uploaded manually), grouped by
`env / service / date / buildTag`, aggregated, and compared across the
function / file / package / overall dimensions.

Two front ends share the same profile engine (`internal/engine`):

- **`mycelia`** — a web UI + JSON API.
- **`mycelia-mcp`** — a [Model Context Protocol](https://modelcontextprotocol.io)
  server that lets AI agents (Claude Code, Claude Desktop) browse and compare
  profile data over stdio.

To *produce* the profiles, drop the single-file Node.js sampler in
[`examples/auto-profiler/`](examples/auto-profiler/) into your service — it
emits `.cpuprofile` files in the `env/service/date/buildTag` layout below, with a
pluggable storage adapter (no npm install required).

## Configuration

Both binaries read the same flags (with env-var fallbacks):

| Flag | Env | Default | Purpose |
|---|---|---|---|
| `-bucket` | `AUTO_PROFILER_BUCKET` | — | GCS bucket name |
| `-key` | `AUTO_PROFILER_KEY_FILE` | — | service-account JSON key path |
| `-root` | `AUTO_PROFILER_ROOT_PATH` | — | object-key root prefix |
| `-sample` | — | `40` | max profiles processed per group (0 = all) |
| `-fetch-concurrency` | — | `24` | concurrent object downloads/parses |
| `-cache-dir` | `MYCELIA_CACHE_DIR` | — | persist per-object aggregations (empty = memory only) |
| `-block-threshold` | — | `50` | event-loop long-task threshold in **ms** (a non-idle span this long is a blocking episode); folded into the cache key |
| `-addr` | `MYCELIA_ADDR` | `:8080` | HTTP listen address (`mycelia` only) |

Without `-bucket`/`-key`, `mycelia` runs in upload-only mode; `mycelia-mcp` has
no profiles to serve (uploads aren't exposed over MCP).

## Web server

```sh
go run ./cmd/mycelia -bucket my-bucket -key sa.json -root reports/
# open http://localhost:8080
```

## MCP server

```sh
go build -o mycelia-mcp ./cmd/mycelia-mcp
```

It speaks MCP over **stdio**, so the host launches it as a subprocess. All
logging goes to stderr; stdout carries the protocol.

### Tools (all read-only)

- **`browse_profiles`** — drill the `env → service → date → buildTag` hierarchy.
  Call with no arguments to list environments; pass `env`, then `env`+`service`,
  then `env`+`service`+`date` to reach leaf groups. Leaf rows are slim:
  `buildTag`, `profileCount`, and the `firstTs`/`lastTs` span (no member dump).
- **`get_group`** — one group's headline metrics plus its top hotspots for a
  `dimension` (`overall|package|function|file|context`) ranked by a `metric`
  (`selfMicros|totalMicros|selfSamples|totalSamples|selfPct|totalPct|`
  `selfPctBusy|totalPctBusy`; default `selfPctBusy`), optionally filtered by
  `categories` (`native|node_modules|user|idle`). The `*PctBusy` metrics are
  shares of non-idle CPU, so composition compares independent of load; the
  summary also reports `idlePct`/`busyPct`.
- **`compare_groups`** — the same, across two or more groups side by side.
  Default metric is `selfPctBusy` (load-independent share of non-idle CPU);
  use `selfMicros` for raw cost. Each row carries `delta`/`deltaPct` (last
  group vs first); `sort` (`max|delta|deltaPct`) ranks rows to surface
  regressions (newly-appeared entities rank first under `deltaPct`).
- **`get_function_breakdown`** — a hot function's immediate callers and callees
  (by inclusive cost), to root-cause a hot path without leaving the profile. By
  default (`stitchAsync`) callers are resolved through async/native trampoline
  frames (`runMicrotasks`, kareem `syncWrapper`, …) to the nearest meaningful
  frame and marked `viaAsync` (the attribution is proportional, not exact); pass
  `stitchAsync:false` for the raw immediate callers. When the profiles carry
  async-context data (the auto-profiler's `_async` block), a `contexts` list
  gives the logical owners (route/job) driving the function by *real* attribution
  — the reliable answer to "which route drives this" across the async gap. Each
  context row also carries `pctOfFunction` (its share of the function's inclusive
  time) and `pctOfContext` (the function's share of *that route's own* busy CPU —
  high means the function accounts for much of the route, so optimizing it saves
  the route proportionally more). `contextSort` (`micros|pctOfContext`) ranks the
  routes by absolute time or by that share, so the best optimization target for a
  route surfaces in one call.
- **`get_breakdown`** — the same idea for a non-function entity (`dimension` =
  `package|file|context`, `key` from a matching `get_group`/`compare_groups`
  row). A **package** returns its member functions and files (by self micros,
  which partition cleanly) plus the contexts that exercise it; a **file** returns
  its functions and contexts; a **context** (route/job) returns the functions
  running under it (inclusive) and the packages and files its self time lands in
  (which sum to the context total) — i.e. *where a route's CPU goes*. Rows that
  pair an entity with a route carry `pctOfContext` (the entity's share of that
  route's own CPU).

- **`get_event_loop_blocking`** — find *where the loop is blocked*: the
  synchronous **long tasks** (uninterrupted non-idle spans ≥ `-block-threshold`,
  default 50 ms) that stall the event loop, attributed three ways — `functions`
  (the leaf code that ran inside long tasks, by blocked micros), `contexts` (the
  async label / route / API that owns the blocking, when profiles carry
  context data), and `stalls` (the worst individual episodes, each with its
  duration, owning context, and full root→leaf call stack). This is distinct
  from `get_group`'s CPU hotspots: a function can be cheap overall yet block the
  loop in rare bursts, or hot yet harmless when its time is spread across short
  ticks. `topN` caps each list; `blockedMicros` are group totals,
  `episodesPerProfile`/`blockedMicrosPerProfile` are per-profile averages, and
  `stalls` are absolute worst-cases.

The `context` dimension, breakdown `contexts`, the per-API blocking `contexts`,
and a context's package/file
composition require profiles captured with context attribution enabled — see
[`examples/auto-profiler`](examples/auto-profiler/). In the web UI, clicking any
package / file / function / context row opens a breakdown popup with the same
sections, and names inside it are themselves drillable.

`topN` caps returned rows (default 25, max 100) so results stay within MCP
output limits. `get_group`/`compare_groups` also accept `from`/`to` (RFC3339) to
restrict to a peak/off-peak time window. Numeric values in MCP responses are
rounded (micros to integers, percentages to 2 dp). Invalid `dimension`/`metric`/
`sort`/`categories` values return an error listing the allowed set rather than
silently defaulting.

### Host config

Add to your MCP host (e.g. `claude_desktop_config.json`, or via
`claude mcp add` in Claude Code):

```json
{
  "mcpServers": {
    "mycelia": {
      "command": "/path/to/mycelia-mcp",
      "args": ["-bucket", "my-bucket", "-key", "/path/to/sa.json", "-root", "reports/"]
    }
  }
}
```

Credentials can also be supplied via the env vars above instead of `args`.
