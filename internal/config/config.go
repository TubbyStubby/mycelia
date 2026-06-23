// Package config loads the mycelia server configuration from flags and
// environment variables.
package config

import (
	"flag"
	"os"
)

// Config holds the server configuration.
type Config struct {
	Addr     string // HTTP listen address
	Bucket   string // GCS bucket name
	KeyFile  string // service-account JSON key file path
	RootPath string // opaque object-key root prefix (matches auto-profiler AUTO_PROFILER_ROOT_PATH)

	// MaxUploadBytes bounds a single upload request's total size.
	MaxUploadBytes int64

	// SampleSize caps how many profiles are processed per group (0 = all).
	// Selection is deterministic (hash-based) so results are reproducible and
	// caches stay warm.
	SampleSize int

	// FetchConcurrency bounds concurrent object downloads/parses. Downloads are
	// I/O-bound, so this is typically higher than the CPU count.
	FetchConcurrency int

	// CacheDir, when set, persists per-object aggregations to disk so they
	// survive restarts (GCS objects are immutable). Empty = in-memory only.
	CacheDir string

	// BlockThresholdMicros is the event-loop long-task threshold: a run of
	// non-idle samples reaching this wall-time counts as a blocking episode. It
	// is baked into each cached aggregation, so the object cache keys on it.
	BlockThresholdMicros int64
}

// GCSEnabled reports whether enough config is present to talk to GCS.
func (c Config) GCSEnabled() bool {
	return c.Bucket != "" && c.KeyFile != ""
}

// Load parses flags (falling back to environment variables) into a Config.
func Load(args []string) (Config, error) {
	fs := flag.NewFlagSet("mycelia", flag.ContinueOnError)

	cfg := Config{}
	fs.StringVar(&cfg.Addr, "addr", env("MYCELIA_ADDR", ":8080"), "HTTP listen address")
	fs.StringVar(&cfg.Bucket, "bucket", env("AUTO_PROFILER_BUCKET", ""), "GCS bucket name")
	fs.StringVar(&cfg.KeyFile, "key", env("AUTO_PROFILER_KEY_FILE", ""), "service-account JSON key file path")
	fs.StringVar(&cfg.RootPath, "root", env("AUTO_PROFILER_ROOT_PATH", ""), "object-key root prefix")
	fs.Int64Var(&cfg.MaxUploadBytes, "max-upload", 256<<20, "max total upload size in bytes")
	fs.IntVar(&cfg.SampleSize, "sample", 40, "max profiles processed per group (0 = all)")
	fs.IntVar(&cfg.FetchConcurrency, "fetch-concurrency", 24, "concurrent object downloads/parses")
	fs.StringVar(&cfg.CacheDir, "cache-dir", env("MYCELIA_CACHE_DIR", ""), "directory to persist per-object aggregations (empty = memory only)")
	blockMs := fs.Int64("block-threshold", 50, "event-loop long-task threshold in ms (a non-idle span this long is a blocking episode)")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	cfg.BlockThresholdMicros = *blockMs * 1000
	return cfg, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
