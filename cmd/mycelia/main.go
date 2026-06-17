// Command mycelia serves a web UI to compare V8 CPU profiles produced by the
// Node.js auto-profiler, loading them from GCS or via manual upload.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/TubbyStubby/mycelia/internal/cache"
	"github.com/TubbyStubby/mycelia/internal/config"
	"github.com/TubbyStubby/mycelia/internal/engine"
	"github.com/TubbyStubby/mycelia/internal/httpapi"
	"github.com/TubbyStubby/mycelia/internal/store"
)

func main() {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Fatalf("config: %v", err)
	}

	ctx := context.Background()

	var gcs *store.GCSSource
	if cfg.GCSEnabled() {
		gcs, err = store.NewGCSSource(ctx, cfg.Bucket, cfg.KeyFile, cfg.RootPath)
		if err != nil {
			log.Fatalf("gcs: %v", err)
		}
		defer gcs.Close()
		log.Printf("GCS source ready: bucket=%s root=%q", cfg.Bucket, cfg.RootPath)
	} else {
		log.Printf("GCS not configured (need -bucket and -key); upload-only mode")
	}

	uploads := store.NewUploadSource()
	c := cache.New()
	objCache, err := cache.NewObjectCache(cfg.CacheDir)
	if err != nil {
		log.Fatalf("object cache: %v", err)
	}
	if cfg.CacheDir != "" {
		log.Printf("per-object cache persisting to %s", cfg.CacheDir)
	}
	if cfg.SampleSize > 0 {
		log.Printf("sampling up to %d profiles per group", cfg.SampleSize)
	}
	eng := engine.New(cfg, gcs, uploads, c, objCache)
	srv := httpapi.New(cfg, eng)

	log.Printf("mycelia listening on %s", cfg.Addr)
	if err := http.ListenAndServe(cfg.Addr, srv.Handler()); err != nil {
		log.Fatalf("server: %v", err)
	}
}
