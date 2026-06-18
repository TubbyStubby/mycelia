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

## Async-context attribution (mycelia "Tier 2a")

V8 CPU profiles record only the **synchronous** sampled stack. Across an `await`
the stack resets, so a continuation's parent is the microtask runner or a library
trampoline rather than the logical caller — which is why mycelia's
`get_function_breakdown` can only *stitch* (approximate) async callers post-hoc.

This profiler closes that gap **at capture time** by attributing each CPU sample
to the logical work that was active — the route/job/query name your app already
carries in an `AsyncLocalStorage`. While a profiling window is open it tracks,
via `async_hooks`, which label was executing over wall-clock time, then joins the
CPU samples to that timeline **in-process** (V8's profile clock and
`process.hrtime` are the same `CLOCK_MONOTONIC`, so no correlation is needed).
The result rides along in the uploaded profile:

```jsonc
"_async": {
  "version": 1,
  "labels": ["GET /match/:id", "job:recomputeRatings", ...],
  "samples": [0, 0, -1, 1, ...]   // parallel to profile.samples; index into labels, -1 = unattributed
}
```

mycelia (Tier 2 consumer support) reads this to answer "which route/job drives
this hot function" directly, instead of guessing across the async gap.

### Enabling it

Provide the profiler a `context` with the app's `AsyncLocalStorage`:

```js
const { AutoProfiler, createContextProvider } = require("./auto-profiler");

// If you already run requests inside an ALS, pass it (and a label extractor):
//   context: { als: myRequestAls, label: store => store?.route }
// Otherwise use the bundled helper:
const ctx = createContextProvider();
app.use(ctx.express(req => `${req.method} ${req.route?.path ?? req.path}`));

const profiler = createFromEnv({ context: { als: ctx.als } });
profiler?.start();
```

The label is whatever string identifies the unit of work; keep it low-cardinality
(route templates, job names — not per-user values).

### Overhead

Context capture turns on `async_hooks` **only during each profiling window**, not
always. The benchmark (`tier2-context.bench.js`) reports ~99% correct attribution
but also a large overhead % — that figure is a **pathological upper bound**: it
measures async machinery with zero real work. Real overhead is proportional to
async-operation density, so a handler doing real CPU/IO sees a small fraction of
that. Two honest caveats remain: (1) it perturbs the very measurement (the async
plumbing samples hotter during the window), and (2) you should measure the real
cost in your service before enabling fleet-wide. It is **opt-in** for these
reasons. Reproduce the numbers with:

```sh
node tier2-context.bench.js        # AUTO_PROFILER_DURATION_MS=2000 for a quicker run
```

### What's deferred (Tier 2b)

Full async **call-stack** reconstruction (rebuilding the exact caller chain across
every await via the `async_hooks` trigger graph + creation-site stacks) is a
bigger, costlier lift. Context attribution above answers the "which owner drives
this" question that motivated Tier 2; 2b is only worth it if labels prove too
coarse.
