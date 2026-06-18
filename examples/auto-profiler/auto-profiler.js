"use strict";

// Automatic V8 CPU profiler — drop-in, single-file, zero npm install required.
//
// This is the *producer* side that emits the `.cpuprofile` files mycelia
// consumes. Copy this file into your Node service and wire it up; it has no
// dependency on mycelia and only one pluggable dependency (a storage adapter).
//
// It is deliberately framework-agnostic: anything project-specific (where to
// upload, how to log, how to label the service/env) is injected, so you can
// paste it into any codebase. See README.md in this folder for the contract.

const inspector = require("inspector");
const os = require("os");

const DEFAULT_MIN_INTERVAL_MS = 15 * 60 * 1000;
const DEFAULT_MAX_INTERVAL_MS = 60 * 60 * 1000;
const DEFAULT_DURATION_MS = 5 * 60 * 1000;
const STOP_AWAIT_TIMEOUT_MS = 5 * 1000;
const DEFAULT_SAMPLE_RATE = 0.1; // collect ~10% of scheduled sessions

/**
 * The one project-specific dependency, expressed as an interface. Provide any
 * object with an `upload` method; the bundled `createGcsStorage` below is one
 * implementation, but you can drop in S3, Azure Blob, local disk, etc.
 *
 * @typedef {Object} ProfileUpload
 * @property {Buffer|string} content     serialized profile JSON
 * @property {string}        name        file name, e.g. "<ms>_<host>_<pid>.cpuprofile"
 * @property {string}        subPath     path prefix within the storage root (ends with "/")
 * @property {string}        contentType MIME type ("application/json")
 *
 * @typedef {Object} ProfileStorage
 * @property {(u: ProfileUpload) => Promise<void>} upload  persist one profile object
 */

/**
 * Optional logger interface. Anything with a `warning(msg)` or `warn(msg)`
 * method works; defaults to the console.
 *
 * @typedef {Object} Logger
 * @property {(msg: string) => void} [warning]
 * @property {(msg: string) => void} [warn]
 */

/**
 * Automatic CPU profiler that periodically samples the process and uploads the
 * resulting profile through an injected storage adapter.
 *
 * Designed for production use with minimal overhead:
 *   - Short sampling windows with long random gaps
 *   - Random intervals spread load across auto-scaling pods naturally
 *   - Each session opens and closes its own inspector Session to avoid persistent overhead
 *   - Upload errors are swallowed (after one warn log) so the profiler never crashes the host process
 *
 * Uploads follow the path layout mycelia expects:
 *   {storageRoot}profiles/{env}/{service}/{YYYY-MM-DD}/{buildTag}/{timestampMs}_{hostname}_{pid}.cpuprofile
 * (the `{storageRoot}` prefix is owned by the storage adapter, not this class.)
 *
 * @example
 * const { createFromEnv } = require("./auto-profiler");
 * const profiler = createFromEnv();          // uses bundled GCS adapter from env
 * profiler?.start();
 * // on graceful shutdown:
 * await profiler?.stop();
 *
 * @example
 * // Bring your own storage (no @google-cloud/storage needed):
 * const { AutoProfiler } = require("./auto-profiler");
 * const profiler = new AutoProfiler({
 *   storage: { async upload({ content, name, subPath }) { ...your blob put... } },
 *   service: "web-server", env: "production", buildTag: "abc123-42",
 * });
 * profiler.start();
 */
class AutoProfiler {
    /** @type {NodeJS.Timeout | null} */
    _timer = null;
    /** @type {Promise<void> | null} */
    _inFlight = null;
    _started = false;
    _loggedUploadError = false;

    /** @type {ProfileStorage} */
    _storage;
    /** @type {Logger} */
    _logger;
    _service;
    _env;
    _buildTag;
    _minIntervalMs;
    _maxIntervalMs;
    _durationMs;
    _sampleRate;

    /**
     * @param {object} opts
     * @param {ProfileStorage} opts.storage         storage adapter implementing `upload()` (required)
     * @param {Logger}  [opts.logger=console]       logger for the single upload-failure warning
     * @param {string}  [opts.service="unknown"]    service label used in the upload path
     * @param {string}  [opts.env="unknown"]        environment label used in the upload path
     * @param {string}  [opts.buildTag="unknown"]   build tag (e.g. shortSha-buildNumber) used in the upload path
     * @param {number}  [opts.minIntervalMs=900000]    min delay between sessions (15 min)
     * @param {number}  [opts.maxIntervalMs=3600000]   max delay between sessions (60 min)
     * @param {number}  [opts.durationMs=300000]       how long to profile per session (5 min)
     * @param {number}  [opts.sampleRate=0.1]          probability [0..1] a scheduled session actually collects/uploads
     */
    constructor({
        storage, logger,
        service = "unknown", env = "unknown", buildTag = "unknown",
        minIntervalMs = DEFAULT_MIN_INTERVAL_MS,
        maxIntervalMs = DEFAULT_MAX_INTERVAL_MS,
        durationMs = DEFAULT_DURATION_MS,
        sampleRate = DEFAULT_SAMPLE_RATE,
    }) {
        if (!storage || typeof storage.upload !== "function") {
            throw new Error("AutoProfiler: `storage` with an upload({ content, name, subPath, contentType }) method is required");
        }
        this._storage = storage;
        this._logger = logger || console;
        this._service = service;
        this._env = env;
        this._buildTag = buildTag;
        this._minIntervalMs = minIntervalMs;
        this._maxIntervalMs = maxIntervalMs;
        this._durationMs = durationMs;
        this._sampleRate = sampleRate;
    }

    /** Begin the automatic profiling loop. Safe to call multiple times. */
    start() {
        if (this._started) return;
        this._started = true;
        this._scheduleNext();
    }

    /**
     * Stop scheduling future sessions and await any in-progress session (capped).
     * @returns {Promise<void>}
     */
    async stop() {
        this._started = false;
        if (this._timer) {
            clearTimeout(this._timer);
            this._timer = null;
        }
        if (this._inFlight) {
            const timeout = new Promise(resolve => setTimeout(resolve, STOP_AWAIT_TIMEOUT_MS));
            await Promise.race([this._inFlight.catch(() => { }), timeout]);
        }
    }

    // ── private ──────────────────────────────────────────────────────────────

    _scheduleNext() {
        if (!this._started) return;
        const delay = this._minIntervalMs + Math.random() * (this._maxIntervalMs - this._minIntervalMs);
        this._timer = setTimeout(() => this._runSession(), delay);
        // Unref so the timer does not prevent the process from exiting naturally
        this._timer.unref();
    }

    _runSession() {
        // Skip if a session is already in progress (shouldn't happen with long intervals)
        if (this._inFlight) {
            this._scheduleNext();
            return;
        }
        // Randomised sampling: only a fraction of scheduled sessions actually collect/upload.
        // Across a large fleet this cuts total profile volume without changing per-pod cadence.
        if (Math.random() >= this._sampleRate) {
            this._scheduleNext();
            return;
        }
        this._inFlight = (async () => {
            try {
                const buffer = await this._collectProfile();
                await this._upload(buffer);
                this._loggedUploadError = false;
            } catch (err) {
                this._logUploadErrorOnce(err);
            } finally {
                this._inFlight = null;
                this._scheduleNext();
            }
        })();
    }

    /** Runs the V8 CPU profiler for durationMs and returns the serialised profile as a Buffer. */
    async _collectProfile() {
        const session = new inspector.Session();
        session.connect();
        try {
            await sessionPost(session, "Profiler.enable");
            await sessionPost(session, "Profiler.start");
            await sleep(this._durationMs);
            const { profile } = await sessionPost(session, "Profiler.stop");
            return Buffer.from(JSON.stringify(profile));
        } finally {
            try { session.disconnect(); } catch (_) { /* ignore */ }
        }
    }

    async _upload(content) {
        const now = new Date();
        const date = now.toISOString().slice(0, 10); // YYYY-MM-DD
        const subPath = `profiles/${this._env}/${this._service}/${date}/${this._buildTag}/`;
        const name = `${now.getTime()}_${os.hostname()}_${process.pid}.cpuprofile`;
        await this._storage.upload({ content, name, subPath, contentType: "application/json" });
    }

    _logUploadErrorOnce(err) {
        if (this._loggedUploadError) return;
        this._loggedUploadError = true;
        const msg = `auto-profiler: upload failed (subsequent failures suppressed): ${err?.message || err}`;
        const log = this._logger || console;
        if (typeof log.warning === "function") log.warning(msg);
        else if (typeof log.warn === "function") log.warn(msg);
        else console.warn(msg);
    }
}

// ── helpers ───────────────────────────────────────────────────────────────────

/** Promisify inspector.Session#post */
function sessionPost(session, method, params = {}) {
    return new Promise((resolve, reject) => {
        session.post(method, params, (err, result) => {
            if (err) reject(err);
            else resolve(result);
        });
    });
}

function sleep(ms) {
    return new Promise(resolve => setTimeout(resolve, ms));
}

/**
 * Parse a sample rate string into a probability in [0, 1]. Returns the default
 * when unset/invalid; clamps out-of-range values. An explicit 0 is honoured.
 * @param {string | undefined} raw
 * @param {number} [def=DEFAULT_SAMPLE_RATE]
 * @returns {number}
 */
function parseSampleRate(raw, def = DEFAULT_SAMPLE_RATE) {
    if (raw === undefined || raw === "") return def;
    const n = Number(raw);
    if (!Number.isFinite(n)) return def;
    return Math.min(1, Math.max(0, n));
}

// ── bundled GCS storage adapter (optional) ──────────────────────────────────────

/**
 * Reference {@link ProfileStorage} backed by Google Cloud Storage. Provided for
 * convenience so GCS users get a true drop-in; it lazily requires
 * `@google-cloud/storage` only when called, so projects using a different
 * backend never need that package installed. Replace with your own adapter to
 * target any other blob store.
 *
 * @param {object} cfg
 * @param {string}  cfg.bucketName
 * @param {string}  [cfg.projectId]    falls back to ADC / GOOGLE_CLOUD_PROJECT
 * @param {string}  [cfg.keyFilename]  path to a service-account JSON (omit for ADC)
 * @param {string}  [cfg.rootPath=""]  prefix prepended to every object name (verbatim, no forced slash)
 * @returns {ProfileStorage}
 */
function createGcsStorage({ bucketName, projectId, keyFilename, rootPath = "" }) {
    if (!bucketName) throw new Error("createGcsStorage: bucketName is required");
    // Lazy require: only GCS users pay for @google-cloud/storage.
    const { Storage } = require("@google-cloud/storage");
    const bucket = new Storage({ projectId, keyFilename }).bucket(bucketName);
    return {
        async upload({ content, name, subPath = "", contentType = "application/json" }) {
            const objectName = `${rootPath}${subPath}${name}`;
            await bucket.file(objectName).save(content, { contentType, resumable: false });
        },
    };
}

// ── factory ───────────────────────────────────────────────────────────────────

/**
 * Create an AutoProfiler configured from environment variables. Returns null
 * when AUTO_PROFILER_ENABLED is not "true", so callers can simply do:
 *
 *   createFromEnv()?.start();
 *
 * Storage resolution (first match wins):
 *   1. opts.storage                       — your adapter, used as-is
 *   2. opts.createStorage(storageConfig)  — your factory, given the resolved config
 *   3. the bundled GCS adapter            — requires AUTO_PROFILER_BUCKET (and @google-cloud/storage)
 *
 * Env vars:
 *   AUTO_PROFILER_ENABLED=true            (required to do anything)
 *   AUTO_PROFILER_BUCKET                  GCS bucket (only for the bundled adapter)
 *   AUTO_PROFILER_GCP_PROJECT             GCS project ID (falls back to GOOGLE_CLOUD_PROJECT)
 *   AUTO_PROFILER_KEY_FILE                path to service-account JSON (omit to use ADC)
 *   AUTO_PROFILER_ROOT_PATH               object-name prefix in the bucket (default: "")
 *   AUTO_PROFILER_SERVICE                 service label (falls back to SERVICE, then "unknown")
 *   AUTO_PROFILER_ENV                     env label   (falls back to NODE_ENV, then "unknown")
 *   AUTO_PROFILER_BUILD_TAG              build tag    (falls back to BUILD_TAG, then "unknown")
 *   AUTO_PROFILER_MIN_INTERVAL_MS         (default: 900000 — 15 min)
 *   AUTO_PROFILER_MAX_INTERVAL_MS         (default: 3600000 — 60 min)
 *   AUTO_PROFILER_DURATION_MS             (default: 300000 — 5 min)
 *   AUTO_PROFILER_SAMPLE_RATE             probability [0..1] a scheduled session uploads (default: 0.1)
 *
 * @param {object} [opts]
 * @param {ProfileStorage} [opts.storage]                       provide your own storage adapter
 * @param {(cfg: object) => ProfileStorage} [opts.createStorage] build a storage adapter from resolved config
 * @param {Logger} [opts.logger]                                logger override
 * @param {string} [opts.service]                               override the service label
 * @param {string} [opts.env]                                   override the env label
 * @param {string} [opts.buildTag]                              override the build tag
 * @returns {AutoProfiler | null}
 */
function createFromEnv(opts = {}) {
    if (process.env.AUTO_PROFILER_ENABLED !== "true") return null;

    const service = opts.service || process.env.AUTO_PROFILER_SERVICE || process.env.SERVICE || "unknown";
    const env = opts.env || process.env.AUTO_PROFILER_ENV || process.env.NODE_ENV || "unknown";
    const buildTag = opts.buildTag || process.env.AUTO_PROFILER_BUILD_TAG || process.env.BUILD_TAG || "unknown";

    const storageConfig = {
        bucketName: process.env.AUTO_PROFILER_BUCKET,
        projectId: process.env.AUTO_PROFILER_GCP_PROJECT || process.env.GOOGLE_CLOUD_PROJECT,
        keyFilename: process.env.AUTO_PROFILER_KEY_FILE || undefined,
        rootPath: process.env.AUTO_PROFILER_ROOT_PATH || "",
    };

    let storage = opts.storage;
    if (!storage) {
        if (typeof opts.createStorage === "function") {
            storage = opts.createStorage(storageConfig);
        } else if (storageConfig.bucketName) {
            storage = createGcsStorage(storageConfig);
        } else {
            throw new Error(
                "AutoProfiler: no storage configured — pass opts.storage, opts.createStorage, " +
                "or set AUTO_PROFILER_BUCKET to use the bundled GCS adapter",
            );
        }
    }

    return new AutoProfiler({
        storage,
        logger: opts.logger,
        service,
        env,
        buildTag,
        minIntervalMs: Number(process.env.AUTO_PROFILER_MIN_INTERVAL_MS) || DEFAULT_MIN_INTERVAL_MS,
        maxIntervalMs: Number(process.env.AUTO_PROFILER_MAX_INTERVAL_MS) || DEFAULT_MAX_INTERVAL_MS,
        durationMs: Number(process.env.AUTO_PROFILER_DURATION_MS) || DEFAULT_DURATION_MS,
        sampleRate: parseSampleRate(process.env.AUTO_PROFILER_SAMPLE_RATE),
    });
}

module.exports = { AutoProfiler, createFromEnv, createGcsStorage, parseSampleRate };
