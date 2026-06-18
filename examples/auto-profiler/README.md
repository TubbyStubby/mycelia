# auto-profiler (producer side)

A single-file, dependency-light **V8 CPU profiler** for Node.js services. It
periodically samples the running process and uploads each `.cpuprofile` to a
storage backend in the path layout [mycelia](../../README.md) expects:

```
{storageRoot}profiles/{env}/{service}/{YYYY-MM-DD}/{buildTag}/{timestampMs}_{hostname}_{pid}.cpuprofile
```

It has **no dependency on mycelia** and nothing project-specific baked in — copy
[`auto-profiler.js`](./auto-profiler.js) into your service and wire it up. No npm
package to install (the one optional dependency, `@google-cloud/storage`, is
lazily required only if you use the bundled GCS adapter).

## Design

- Short sampling windows (default 5 min) with long random gaps (15–60 min).
- Random intervals spread load naturally across auto-scaling pods.
- `sampleRate` (default 0.1) drops most scheduled sessions so a large fleet emits
  far fewer profiles without changing per-pod cadence.
- Each session opens/closes its own `inspector.Session` — no persistent overhead.
- Upload errors are swallowed after one warning, so profiling never crashes the host.

## The one injected dependency: storage

Everything project-specific is injected. The storage adapter is any object with
an `upload` method:

```js
/** @type {ProfileStorage} */
const storage = {
  async upload({ content, name, subPath, contentType }) {
    // content: Buffer (profile JSON)
    // name:    "<ms>_<host>_<pid>.cpuprofile"
    // subPath: "profiles/<env>/<service>/<date>/<buildTag>/"
    // put the object at  <yourRoot> + subPath + name
  },
};
```

This is the seam to replace for S3, Azure Blob, local disk, an internal storage
wrapper, etc. The `{storageRoot}` prefix is owned by the adapter, not the
profiler.

## Usage

### Bring your own storage (zero extra deps)

```js
const { AutoProfiler } = require("./auto-profiler");

const profiler = new AutoProfiler({
  storage,                    // your adapter (above)
  service: "web-server",
  env: "production",
  buildTag: process.env.BUILD_TAG,
  // logger: yourLogger,      // optional; needs .warning() or .warn(); defaults to console
});
profiler.start();

// on graceful shutdown:
await profiler.stop();
```

### Env-driven, bundled GCS adapter

```js
const { createFromEnv } = require("./auto-profiler");
createFromEnv()?.start();     // returns null unless AUTO_PROFILER_ENABLED=true
```

Storage resolution in `createFromEnv` (first match wins):

1. `opts.storage` — your adapter, used as-is.
2. `opts.createStorage(config)` — your factory, given the resolved config.
3. the bundled GCS adapter — requires `AUTO_PROFILER_BUCKET` and
   `@google-cloud/storage` installed.

### Environment variables

| Var | Purpose | Default |
|---|---|---|
| `AUTO_PROFILER_ENABLED` | must be `"true"` to run | — |
| `AUTO_PROFILER_BUCKET` | GCS bucket (bundled adapter only) | — |
| `AUTO_PROFILER_GCP_PROJECT` | GCS project (falls back to `GOOGLE_CLOUD_PROJECT`) | ADC |
| `AUTO_PROFILER_KEY_FILE` | service-account JSON path (omit for ADC) | ADC |
| `AUTO_PROFILER_ROOT_PATH` | object-name prefix in the bucket | `""` |
| `AUTO_PROFILER_SERVICE` | service label (falls back to `SERVICE`) | `unknown` |
| `AUTO_PROFILER_ENV` | env label (falls back to `NODE_ENV`) | `unknown` |
| `AUTO_PROFILER_BUILD_TAG` | build tag (falls back to `BUILD_TAG`) | `unknown` |
| `AUTO_PROFILER_MIN_INTERVAL_MS` | min gap between sessions | `900000` (15 min) |
| `AUTO_PROFILER_MAX_INTERVAL_MS` | max gap between sessions | `3600000` (60 min) |
| `AUTO_PROFILER_DURATION_MS` | profiling window per session | `300000` (5 min) |
| `AUTO_PROFILER_SAMPLE_RATE` | probability `[0..1]` a session uploads | `0.1` |

## Extending for async stacks (mycelia Tier 2)

V8 CPU profiles record only the **synchronous** sampled stack. Across an `await`
the stack resets, so a continuation's parent is the microtask runner or a library
trampoline rather than the logical caller — which is why mycelia's
`get_function_breakdown` has to *stitch* (approximate) async callers post-hoc.

The exact fix lives **here, at capture time**: correlate continuations with their
initiators (e.g. via `async_hooks` / `AsyncLocalStorage`, or by enabling async
stack capture on the inspector) and emit a parent-async marker per sample. The
profiler would then write an augmented profile that mycelia can join into true
caller chains. This is the bigger Tier-2 lift and is **not implemented yet** —
this vendored file is the prerequisite for it.
