# mycelia-mcp — Improvement Suggestions

Feedback gathered from using the `mycelia-mcp` server in anger during a real
production incident investigation (Node.js `web-server` event-loop latency
regression). Each item below is grounded in concrete friction hit while driving
the three MCP tools (`browse_profiles`, `get_group`, `compare_groups`), with code
pointers, real example payloads, and the expected behaviour.

This document is written so another agent (or engineer) can pick up any item
independently. Items are ordered by impact-to-effort. Each has: **Problem →
Evidence → Where → Proposed change → Example → Effort/Risk**.

---

## Context: how the tools were used

The investigation compared CPU profiles across deploys to find what made the
Node event loop CPU-bound:

- `browse_profiles` to walk `env → service → date → buildTag` and find the
  baseline build (`2026-06-16/1d4e028-3464`) vs the regressed build
  (`2026-06-17/ad8fd5f-3483`, 25 profiles, peak traffic).
- `compare_groups` at `dimension:package` and `dimension:function`, `metric:selfMicros`,
  to diff the two builds.
- `get_group` with `categories:["user"]` to isolate application code from
  library internals.

Key finding the tools helped reach: `mongoose` (16.1% self) + `bson` (10.2% self)
document hydration = ~26% of all CPU; idle dropped 45.9% → 40.5% between builds.
The friction items below are everything that made reaching that conclusion harder
than it needed to be.

---

## What works well (do not regress these)

- **Deterministic sampling + per-object cache** (`internal/profiles/sample.go`,
  `internal/engine/engine.go:230` `buildPlan`) — repeated compares were fast and
  reproducible.
- **Per-profile averaging** (`internal/compare/matrix.go:264` `avg`) — groups of
  different sizes (3 vs 25 profiles) compare fairly. Essential; keep it.
- **`selfPct` stable under category filtering** — the denominator is the group's
  whole self-time (`matrix.go:144`), so `categories:["user"]` still yields
  percentages against the global total. This was the most useful single feature.
- **`profileCount` / `totalProfiles` / `sampleCount`** in the summary — made
  sampling coverage visible.
- **Read-only tool annotations** (`cmd/mycelia-mcp/main.go:85`).

---

## 1. `browse_profiles` leaf dumps every member's full object key (token blowout)

**Problem.** At the `buildTag` (leaf) level, `browse_profiles` returns full
`profiles.Group` objects including the entire `Members` slice — each member's
`raw` GCS object key, `timestamp`, `pid`, `hostname`, `size`. The agent only needs
to *identify* a group (env/service/date/buildTag) and know how many profiles it
has; it never consumes member-level detail (there is no `get_members` MCP tool).

**Evidence.** Browsing `production/web-server/2026-06-17` returned 25 members for
`ad8fd5f-3483`, each like:

```json
{
  "key": {
    "buildTag": "ad8fd5f-3483", "date": "2026-06-17", "env": "production",
    "hostname": "web-server-deployment-6fd88d645b-b5df9", "pid": 1,
    "raw": "ws-cpu-profiles/profiles/production/web-server/2026-06-17/ad8fd5f-3483/1781714904347_web-server-deployment-6fd88d645b-b5df9_1.cpuprofile",
    "service": "web-server", "timestamp": "2026-06-17T16:48:24.347Z"
  },
  "size": 12027254
}
```

This **overflowed the MCP tool-output token limit**; I had to redirect the output
to a file and parse it with python just to read the list of build tags — defeating
the point of an interactive tool.

**Where.** `cmd/mycelia-mcp/main.go:147` `browseHandler` returns the raw
`store.BrowseResult` (`internal/store/store.go:24`), whose `Groups` field carries
`profiles.Group.Members`.

**Proposed change.** Give the MCP browse a slimmed leaf shape — drop `Members`,
keep an aggregate. Either a new DTO in `main.go`, or a `BrowseResult` variant for
MCP. Minimum useful leaf row:

```json
{ "buildTag": "ad8fd5f-3483", "profileCount": 25,
  "firstTs": "2026-06-17T16:48:24Z", "lastTs": "2026-06-17T23:49:54Z" }
```

`firstTs`/`lastTs` are derivable from `Members[].Key.Timestamp` without extra IO
and directly support item #5.

**Effort/Risk.** Low / low. Web UI keeps the full shape; only the MCP handler maps
to the slim DTO.

---

## 2. Round numbers and drop redundant `trend` in MCP output (token economy)

**Problem.** Values are emitted at full float precision and a per-row `Trend`
array duplicates data already in `Cells`. A 35-row × 2-group function compare is
large and mostly wasted tokens.

**Evidence.** Real cells returned:

```json
{ "selfMicros": 138329894.33333334, "selfPct": 45.94913250099677,
  "totalMicros": 138329894.33333334, "totalPct": 45.94913250099677, ... }
```

15 significant digits of microseconds and percent convey nothing actionable.
And every `Row` carries `"trend": [<selfMicros g0>, <selfMicros g1>]`
(`matrix.go:62`,`:162`), which is exactly `cellMetric(cell, metric)` for each cell
the agent already has.

**Where.** `internal/compare/matrix.go:47` (`Cell` fields), `:160-173` (row
assembly, `Trend`). Rounding is cleanest applied where the MCP DTO is serialized
(`cmd/mycelia-mcp/main.go` handlers) so the web API can keep full precision if it
wants.

**Proposed change.**
- Round `*Micros` to whole integers, `*Pct` to 2 decimals, `*Samples` to 1.
- Omit `Trend` in the MCP responses (`groupView` and `compare.Matrix`); it exists
  for the web sparkline, not for an agent.

**Example (after).**

```json
{ "selfMicros": 138329894, "selfPct": 45.95, "totalMicros": 138329894, "totalPct": 45.95 }
```

**Effort/Risk.** Low / low. Pure presentation; no analytic change.

---

## 3. Make `compare_groups` regression-aware (rank by delta)

**Problem.** Rows are ranked by `rowMax` — the **maximum** of the metric across
groups (`matrix.go:175`,`:298`). For "what changed between build A and B" this is
the wrong sort: a function expensive in *both* builds outranks one that *newly
appeared* or *doubled*. The agent must manually subtract values across all rows to
find regressions.

**Evidence.** To find the regression I manually diffed `selfPct` across 35 rows:
- `bson deserializeObject` 6.85% → 8.00% (grew)
- `express response.js:1145 stringify` **0 → 1.90%** (newly appeared — but it
  ranked low because its *max* across groups is small relative to `(idle)` etc.)
- `mongoose applyDefaults` 0.83% → 0.95%

The newly-appeared `stringify` is the most interesting signal and is exactly what
a max-ranked list buries.

**Where.** `internal/compare/matrix.go:94` `BuildMatrix` (sort at `:175`), plus a
new sort option threaded from `cmd/mycelia-mcp/main.go` inputs
(`getGroupInput`/`compareInput`, `main.go:161`,`:195`).

**Proposed change.**
- Add an optional `sort` parameter: `max` (current default) | `delta` |
  `deltaPct`. `delta` ranks by `lastCell.metric − firstCell.metric`; `deltaPct` by
  relative change.
- Optionally add a computed `delta` and `deltaPct` field to each `Row` (last vs
  first group) so the change is visible without the agent recomputing.

**Example.** `compare_groups(dimension:function, metric:selfPct, sort:"delta")`
should surface, top of list:

```json
{ "display": "stringify (express/lib/response.js:1145)", "delta": 1.90, "trendPct": [0, 1.90] }
{ "display": "deserializeObject (bson/lib/bson.cjs:3072)", "delta": 1.15, "trendPct": [6.85, 8.00] }
```

**Effort/Risk.** Medium / low. Only affects ordering + an added field; existing
default (`max`) preserved.

---

## 4. Surface idle/busy explicitly + a busy-normalized metric

**Problem.** The most decision-relevant number — how much the CPU was *busy* vs
idle — is only available by hunting the `(idle)` row out of `dimension:package`
output. And when two groups are at different traffic levels (off-peak vs peak),
absolute micros aren't comparable and even `selfPct` shifts simply because idle
moves, not because code got more/less expensive.

**Evidence.** The headline conclusion was "idle 45.9% → 40.5%, so ~5.5pt more
busy CPU per unit wall-clock." I derived that by reading the `(idle)` package row
in each group:

```
(idle) selfPct: 45.95  (2026-06-16 / 1d4e028-3464)
(idle) selfPct: 40.46  (2026-06-17 / ad8fd5f-3483)
```

To compare *composition* (did the share of CPU going to mongoose change?) I'd want
percentages of **busy** time, not of wall time. Right now mongoose 14.36% → 16.14%
selfPct is partly inflated just because idle shrank.

**Where.** `internal/compare/matrix.go:69` `GroupSummary` (add fields);
`internal/compare/matrix.go:30-32` (`Metric` enum) for a new busy-normalized
metric; `(idle)` is already a package entity with `Category == "idle"`
(`internal/v8profile/package.go` / `Category`).

**Proposed change.**
- Add `idlePct` and `busyPct` to `GroupSummary` (cheap: idle self / overall self).
- Add a metric `selfPctBusy` (and/or `totalPctBusy`) = self / (overall self − idle
  self) × 100, so CPU composition can be compared independent of load.

**Example.** With `selfPctBusy`, the mongoose+bson story is load-independent:

```
mongoose selfPctBusy: 26.6% → 27.1%   (vs raw selfPct 14.4% → 16.1%)
```

making it clear the *composition* barely moved and the extra lag was load/idle,
not a per-request code change — a sharper, less misleading conclusion.

**Effort/Risk.** Low / low. Summary fields are trivial; the new metric is one more
`case` in `cellMetric` (`matrix.go:279`) plus the denominator.

---

## 5. Time-window filtering / surface the merged time range

**Problem.** Groups are keyed by date+buildTag, but within a day traffic swings
between peak and off-peak. `Sample` (`profiles/sample.go`) draws deterministically
across the *whole day*, so a group blends peak and off-peak profiles and dilutes
the peak-time signal — exactly the window of interest during an incident.

**Evidence.** The incident peak was 18:30–21:00 IST (13:00–15:30 UTC). The
`2026-06-17/ad8fd5f-3483` group spans members from 16:48 UTC to 23:49 UTC — a mix
of peak and late-night low traffic. I could not restrict the aggregation to the
peak window, so the per-profile averages were softened by off-peak profiles.

**Where.** `internal/engine/engine.go:197` `planGroup` (filter members before
`Sample`); `cmd/mycelia-mcp/main.go` inputs (add `from`/`to`); members carry
`Key.Timestamp`.

**Proposed change.**
- Accept optional `from`/`to` (RFC3339) on `get_group`/`compare_groups`; filter
  `group.Members` by `Key.Timestamp` before sampling.
- At minimum (even without filtering), add `firstTs`/`lastTs` of the merged set to
  `GroupSummary` so the agent knows the window it is reasoning over.

**Example.** `get_group(..., from:"2026-06-17T13:00:00Z", to:"2026-06-17T15:30:00Z")`
returns an aggregation built only from peak-window profiles, with the summary
showing `profileCount` reflecting just that slice.

**Effort/Risk.** Medium / low. Cache signature (`cache.MemberSignature`,
`engine.go:223`) already keys on the member set, so a filtered set caches
correctly with no extra work.

---

## 6. Call-graph drill-down (callers/callees) — biggest capability gap

**Problem.** When a function is found hot, the tools give its self vs inclusive
(`total`) cost but not *who calls it* or *where its inclusive time goes*. The agent
must leave the profile and read source to understand the hot path.

**Evidence.** `getPlayerV2 (helpers/LoadDataHelper.js:854)` showed `totalMicros`
≈ 1.6% (inclusive) and `selfMicros` ≈ 0.49% (self). That says "16%… flows through
here" but not whether the inclusive time is its own compute, the Mongoose
hydration it triggers, or a callee like `computeShotAbilities`. I had to open the
file to find the `moment()` calls and repeated strength recalcs. A call-graph view
would have kept the analysis inside the tool.

**Where.** `internal/v8profile/aggregate.go:202` `aggregateTree` currently collapses
the call tree into per-entity self/total and **discards edge information**. Adding
caller/callee breakdowns requires retaining edges (e.g. a `map[parentKey]map[childKey]Metric`)
during the DFS.

**Proposed change.**
- Retain function→function edge weights during aggregation (guard behind a flag or
  a separate aggregation pass to bound memory).
- Add a tool `get_function_breakdown(group, functionKey)` returning immediate
  callers and callees with self/total contribution, e.g.:

```json
{ "function": "getPlayerV2 (helpers/LoadDataHelper.js:854)",
  "callers": [ { "display": "playersDataForMatch (...:1141)", "totalMicros": 2706981 } ],
  "callees": [ { "display": "computeShotAbilities (functions/Player.js:1775)", "totalMicros": 464173 },
               { "display": "applyDefaults (mongoose/.../applyDefaults.js:5)", "totalMicros": 7209269 } ] }
```

**Effort/Risk.** High / medium. The most valuable item for root-causing, but needs
real work in the aggregation core and careful memory bounds. Tackle after the
easy cluster.

---

## 7. Validate enums instead of silently defaulting

**Problem.** Invalid `dimension`/`metric` values silently fall back to defaults
rather than erroring, so a typo yields plausible-looking wrong-axis data.

**Evidence (from code).** `entityMap` (`matrix.go:268`) `default`s any unknown
dimension to `Functions`; `cellMetric` (`matrix.go:279`) `default`s any unknown
metric to `selfMicros`. So `dimension:"pacakge"` (typo) silently returns
function-level data labelled as the requested dimension.

**Where.** `cmd/mycelia-mcp/main.go` handlers (`getGroupHandler:177`,
`compareHandler:203`) before calling `eng.Compare`; or validate inside `engine.Compare`
(`engine.go:147`).

**Proposed change.** Validate `dimension`/`metric`/`categories`/`sort` against the
known sets; on mismatch return an MCP error listing the allowed values. The result
already echoes the *effective* `dimension`/`metric` (`groupView`/`Matrix`), which
is good — but an explicit error beats silent coercion.

**Example.** `get_group(dimension:"pacakge")` → error:
`invalid dimension "pacakge"; allowed: overall|package|function|file`.

**Effort/Risk.** Low / low.

---

## 8. Document sampling in the tool descriptions

**Problem.** The `get_group`/`compare_groups` descriptions don't state that a group
is sampled to `-sample` (default 40) profiles. The agent can't tell from the
description that `profileCount` may be a sample of `totalProfiles`.

**Where.** Tool descriptions in `cmd/mycelia-mcp/main.go:104-111`,`:118-124`.

**Proposed change.** Add one sentence: "Groups larger than the server's sample
size (default 40) are deterministically sampled; the summary's `profileCount` vs
`totalProfiles` shows how many were merged."

**Effort/Risk.** Trivial / none.

---

## Suggested sequencing

1. **Easy cluster (one PR):** #1 (slim browse leaf), #2 (round + drop trend),
   #4 (idle/busy + `selfPctBusy`), #7 (enum validation), #8 (doc sampling).
   These directly remove the token-bloat and cross-load-comparison friction.
2. **#3 (delta-ranked compare)** — high value for the core "diff two builds" use
   case, modest effort.
3. **#5 (time-window filtering)** — peak-vs-off-peak isolation.
4. **#6 (call-graph drill-down)** — largest capability gain; do last, needs
   aggregation-core changes.

---

## Quick reference: code locations cited

| Item | File:line |
|---|---|
| 1 | `cmd/mycelia-mcp/main.go:147` (`browseHandler`); `internal/store/store.go:24` (`BrowseResult`) |
| 2 | `internal/compare/matrix.go:47` (`Cell`), `:62`/`:162` (`Trend`) |
| 3 | `internal/compare/matrix.go:175`/`:298` (sort/`rowMax`); inputs `main.go:161`/`:195` |
| 4 | `internal/compare/matrix.go:69` (`GroupSummary`), `:30`/`:279` (`Metric`/`cellMetric`) |
| 5 | `internal/engine/engine.go:197` (`planGroup`), `:223` (cache sig); inputs in `main.go` |
| 6 | `internal/v8profile/aggregate.go:202` (`aggregateTree`) |
| 7 | `internal/compare/matrix.go:268`/`:279`; handlers `main.go:177`/`:203` |
| 8 | `cmd/mycelia-mcp/main.go:104-124` |
