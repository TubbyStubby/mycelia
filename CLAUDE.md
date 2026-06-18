# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Mycelia compares and analyzes V8 CPU profiles (`.cpuprofile`) emitted by the
Node.js auto-profiler. Profiles live in a GCS bucket under
`<root>/profiles/<env>/<service>/<date>/<buildTag>/<ms>_<host>_<pid>.cpuprofile`,
or are uploaded manually. They are grouped by that `env/service/date/buildTag`
path, parsed, aggregated, and compared across the function / file / package /
overall dimensions.

Two binaries (`cmd/mycelia`, `cmd/mycelia-mcp`) are thin front ends over the
same `internal/engine`. **Most behavior changes belong in the shared packages,
not the `cmd/` mains.**

## Commands

```sh
go build ./cmd/mycelia ./cmd/mycelia-mcp   # build both
go test ./...                              # all tests
go test ./internal/v8profile/             # one package
go test -run TestAggregate ./internal/v8profile/   # one test
go vet ./...
go run ./cmd/mycelia -bucket B -key sa.json -root reports/   # web UI on :8080
```

Both binaries take the same flags / env vars (see README "Configuration").
Without `-bucket`+`-key` they run upload-only (`mycelia` serves the UI;
`mycelia-mcp` has nothing to serve, since uploads aren't exposed over MCP).

## Architecture

Data flows **source → parse → per-profile aggregate → merge per group → compare**,
with two cache layers in between:

- `internal/store` — `ProfileSource` interface with two implementations:
  `GCSSource` (lists/opens objects following the auto-profiler naming
  convention) and `UploadSource` (in-memory, under the virtual `upload` env).
  `engine.sourceFor` routes a group to the right one.
- `internal/profiles` — group identity (`GroupID`, `ObjectKey`), object-key
  parsing, and **deterministic hash-based sampling** (`Sample`). Sampling is
  capped by `-sample` (default 40) and is stable so caches stay warm and results
  reproduce.
- `internal/v8profile` — the core. `ParseProfile` reads the V8 JSON;
  `AggregateProfile` rolls one profile into function/file/package `Entity` maps.
  `DerivePackage`/`Category` (in `package.go`) classify each frame into
  `native|node_modules|user|idle`. `MergeAggregations` sums per-profile
  aggregations into a per-group one.
- `internal/compare` — `BuildMatrix` assembles the N-group side-by-side matrix
  for a dimension+metric, ranked and capped at `topN`.
- `internal/engine` — orchestrates the above: `Browse`, `GroupAggregation`,
  `Compare`. Plans each group (list+sample) up front, then builds with bounded
  concurrency (`-fetch-concurrency`).
- `internal/httpapi` — JSON API + embedded static UI (`web/`). `cmd/mycelia`.
- `cmd/mycelia-mcp` — MCP server over **stdio**, exposing read-only
  `browse_profiles` / `get_group` / `compare_groups` tools backed by the engine.

### Two cache layers (both keyed by immutability)

- `cache.ObjectCache` — caches one parsed+aggregated **object**, keyed by raw
  name + size. GCS objects are immutable, so this is safe; `-cache-dir` persists
  it to disk (gob) across restarts.
- `cache.Cache` — caches a **merged per-group** aggregation, keyed by group id +
  a `MemberSignature` (sha256 of member names+sizes). A changed member set
  (new upload/object) invalidates it. Uses `singleflight` to dedup concurrent
  builds. In-memory only.

When changing what goes into an aggregation or how members are sampled, remember
both cache keys: object cache keys on name+size; group cache keys on the member
signature. Neither keys on the aggregation contents, so a logic change to
`AggregateProfile`/`MergeAggregations` is **not** auto-invalidated — clear
`-cache-dir` when iterating on aggregation logic.

## Aggregation invariants (the subtle part)

These are easy to break; tests in `internal/v8profile` and `internal/compare`
guard them:

- **Self vs Total + recursion collapse.** `aggregateTree` walks the call tree
  DFS. Self time is always additive; inclusive Total adds a node's self once per
  *distinct active key* on the path, so a recursive function isn't
  double-counted. If you touch the traversal, preserve this.
- **Per-profile averages.** `compare` divides summed metrics by the number of
  profiles merged (`avg`), so groups of different sizes — and sampled vs full —
  compare fairly. Percentages are computed from summed values (ratios are
  unaffected by averaging) against the group's *unfiltered* overall self, so
  they stay stable under category filtering.
- **Approximate timing.** Profiles with `samples`+`timeDeltas` give exact
  self-time; legacy profiles fall back to `hitCount` distributed across wall
  duration and set `TimingApproximate`. Surface that flag, don't silently mix.
- **Category filtering partitions by package.** Every frame belongs to exactly
  one package/category, so a filtered overall is the sum of allowed packages.

## Conventions

- `mycelia-mcp` speaks protocol on **stdout**; all logging must go to stderr
  (`log.SetOutput(os.Stderr)`). Never print to stdout from MCP code paths.
- Service-account keys are gitignored (`sa.json`, `*-key.json`, etc.) — keep it
  that way; never commit credentials.
- `web/static` is embedded via `go:embed` (`web/embed.go`); the `mycelia` binary
  is self-contained, no separate asset serving.

`SUGGESTIONS.md` contains a running list of proposed improvements — consult it
for context on planned work, but it is not authoritative.
