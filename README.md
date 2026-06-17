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
  then `env`+`service`+`date` to reach leaf groups.
- **`get_group`** — one group's headline metrics plus its top hotspots for a
  `dimension` (`overall|package|function|file`) ranked by a `metric`
  (`selfMicros|totalMicros|selfSamples|totalSamples|selfPct|totalPct`),
  optionally filtered by `categories` (`native|node_modules|user|idle`).
- **`compare_groups`** — the same, across two or more groups side by side.

`topN` caps returned rows (default 25, max 100) so results stay within MCP
output limits.

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
