"use strict";
// Reproducible benchmark for the auto-profiler's Tier 2a async-context capture.
//
// It answers the two questions that gate the feature:
//   1. Correctness — do CPU samples land on the right logical label? It drives
//      the real AutoProfiler.captureProfile() with context capture enabled, where
//      burnMatch/burnPlayer/burnJob are each only ever called inside their own
//      labelled context, then checks how many of their samples got that label.
//   2. Overhead — what does enabling async_hooks cost? Measured on an async-only
//      microbenchmark (a pathological upper bound; real services see far less).
//
// Run:  node tier2-context.bench.js
//       AUTO_PROFILER_DURATION_MS=2000 node tier2-context.bench.js   # quicker
//
// Reference numbers (Node 25, idle laptop): ~99% correct attribution, ~92% of
// samples attributed. The overhead figure is workload-dependent and large here
// only because the microbenchmark does no real work — see README "Overhead".

const async_hooks = require("async_hooks");
const { AutoProfiler, createContextProvider } = require("./auto-profiler");

const DURATION_MS = Number(process.env.AUTO_PROFILER_DURATION_MS) || 3000;

// ── workload ────────────────────────────────────────────────────────────────
function burn(ms) {
    const end = process.hrtime.bigint() + BigInt(Math.round(ms * 1e6));
    let x = 0; while (process.hrtime.bigint() < end) x += Math.sqrt(x + 1);
    return x;
}
function burnMatch(ms) { return burn(ms); }   // only called under "GET /match"
function burnPlayer(ms) { return burn(ms); }   // only called under "GET /player"
function burnJob(ms) { return burn(ms); }      // only called under "job:recompute"
const ROUTES = [["GET /match", burnMatch], ["GET /player", burnPlayer], ["job:recompute", burnJob]];
const EXPECT = { burnMatch: "GET /match", burnPlayer: "GET /player", burnJob: "job:recompute" };
const tick = () => new Promise(r => setImmediate(r));

async function generateLoad(provider, durationMs) {
    const start = process.hrtime.bigint();
    const elapsedMs = () => Number(process.hrtime.bigint() - start) / 1e6;
    let i = 0;
    while (elapsedMs() < durationMs) {
        const batch = [];
        for (let k = 0; k < 6; k++) {
            const [label, fn] = ROUTES[(i++) % ROUTES.length];
            // await across the boundary, so fn() runs in a detached continuation.
            batch.push(provider.run(label, async () => { await tick(); fn(1); await tick(); fn(1); }));
        }
        await Promise.all(batch);
    }
}

// ── correctness: drive the real AutoProfiler with context capture ────────────
async function measureCorrectness() {
    const provider = createContextProvider();
    let uploaded = null;
    const profiler = new AutoProfiler({
        storage: { async upload({ content }) { uploaded = content; } }, // capture, don't ship
        durationMs: DURATION_MS,
        context: { als: provider.als },
        service: "bench", env: "bench", buildTag: "bench",
    });

    // captureProfile() profiles for durationMs; drive load concurrently.
    const [profile] = await Promise.all([
        profiler.captureProfile(),
        generateLoad(provider, DURATION_MS - 200),
    ]);
    void uploaded;

    const fnName = {};
    for (const n of profile.nodes) fnName[n.id] = n.callFrame.functionName || "(anonymous)";
    const { labels, samples: ctx } = profile._async;

    let attributed = 0, burnTotal = 0, burnCorrect = 0;
    for (let i = 0; i < profile.samples.length; i++) {
        const idx = ctx[i];
        if (idx >= 0) attributed++;
        const fn = fnName[profile.samples[i]];
        if (EXPECT[fn]) {
            burnTotal++;
            if (idx >= 0 && labels[idx] === EXPECT[fn]) burnCorrect++;
        }
    }
    return {
        totalSamples: profile.samples.length,
        labels,
        attributedPct: +(100 * attributed / profile.samples.length).toFixed(1),
        knownCallSiteSamples: burnTotal,
        correctlyAttributedPct: burnTotal ? +(100 * burnCorrect / burnTotal).toFixed(1) : null,
    };
}

// ── overhead: async-only microbenchmark, hooks off vs on ─────────────────────
async function measureOverhead(ops = 40000) {
    const als = createContextProvider().als;
    const labelByAsyncId = new Map();
    const hook = async_hooks.createHook({
        init(id, _t, trigger) { labelByAsyncId.set(id, als.getStore() ?? labelByAsyncId.get(trigger)); },
        before() { /* timeline work elided; we only measure hook dispatch cost */ },
        destroy(id) { labelByAsyncId.delete(id); },
    });
    const run = async () => { for (let i = 0; i < ops; i++) await als.run("r" + (i % 3), async () => { await tick(); await tick(); }); };

    await run(); // warm up
    let t = process.hrtime.bigint(); await run(); const off = Number(process.hrtime.bigint() - t) / 1e6;
    hook.enable();
    t = process.hrtime.bigint(); await run(); const on = Number(process.hrtime.bigint() - t) / 1e6;
    hook.disable();
    return { ops, hooksOffMs: +off.toFixed(1), hooksOnMs: +on.toFixed(1), overheadPct: +(100 * (on - off) / off).toFixed(1) };
}

(async () => {
    const correctness = await measureCorrectness();
    const overhead = await measureOverhead();
    console.log(JSON.stringify({ nodeVersion: process.version, durationMs: DURATION_MS, correctness, overhead }, null, 2));
})().catch(e => { console.error(e); process.exit(1); });
