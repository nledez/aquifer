package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/nledez/aquifer/internal/blobstore"
	"github.com/nledez/aquifer/internal/cache"
	"github.com/nledez/aquifer/internal/fetch"
	"github.com/nledez/aquifer/internal/server"
)

const (
	// readHeaderTimeout bounds how long a client may take to send its request
	// line and headers. It is not a limit on the response.
	readHeaderTimeout = 10 * time.Second
	// idleTimeout closes keep-alive connections that go quiet.
	idleTimeout = 2 * time.Minute
	// shutdownGrace lets in-flight responses finish on the way out.
	shutdownGrace = 30 * time.Second
	// fetchTimeout bounds a single blob download, well above what a 90 MiB
	// object needs on a slow link.
	fetchTimeout = 30 * time.Minute
)

func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr, "Usage: aquifer serve [flags]\n\n"+
			"Serves apt clients from a local cache backed by object storage.\n\n")
		fs.PrintDefaults()
	}

	configPath := fs.String("config", "", "configuration file (env AQUIFER_CONFIG)")
	listen := fs.String("listen", "", "client-facing address (overrides the file and the environment)")
	adminListen := fs.String("admin-listen", "", "address for /metrics, /healthz and /readyz")
	cacheDir := fs.String("cache-dir", "", "cache directory")
	logFormat := fs.String("log-format", "", "json or text")
	logLevel := fs.String("log-level", "", "debug, info, warn or error")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	path := findConfig(*configPath)
	if path == "" {
		_, _ = fmt.Fprintln(stderr, "aquifer serve: no configuration file found; pass -config or set AQUIFER_CONFIG")
		return 2
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "aquifer serve: %v\n", err)
		return 2
	}
	if err := cfg.ApplyEnv(envLookup); err != nil {
		_, _ = fmt.Fprintf(stderr, "aquifer serve: %v\n", err)
		return 2
	}

	// Flags are the last word, per SPEC section 9.
	overrideString(&cfg.Listen, *listen)
	overrideString(&cfg.AdminListen, *adminListen)
	overrideString(&cfg.Cache.Dir, *cacheDir)
	overrideString(&cfg.Log.Format, *logFormat)
	overrideString(&cfg.Log.Level, *logLevel)

	if err := cfg.Validate(); err != nil {
		_, _ = fmt.Fprintf(stderr, "aquifer serve: %v\n", err)
		return 2
	}

	log := newLogger(stderr, cfg.Log.Format != "text", parseLevel(cfg.Log.Level))
	if err := serve(ctx, cfg, log, stdout); err != nil {
		log.Error("aquifer stopped", "error", err)
		return 1
	}
	return 0
}

func serve(ctx context.Context, cfg *Config, log *slog.Logger, stdout io.Writer) error {
	store, err := blobstore.NewS3(blobstore.Config{
		Endpoint:  cfg.S3.Endpoint,
		Bucket:    cfg.S3.Bucket,
		Prefix:    cfg.S3.Prefix,
		Region:    cfg.S3.Region,
		PathStyle: cfg.S3.PathStyle,
		Insecure:  cfg.S3.Insecure,
		AccessKey: cfg.S3.AccessKey,
		SecretKey: cfg.S3.SecretKey,
	})
	if err != nil {
		return err
	}

	blobCache, err := cache.New(cache.Config{
		Dir:           cfg.Cache.Dir,
		MaxSize:       int64(cfg.Cache.MaxSize),
		PinnedMaxSize: int64(cfg.Cache.PinnedMaxSize),
		TempReserve:   int64(cfg.Cache.TempReserve),
		Logger:        log,
	})
	if err != nil {
		return err
	}
	defer func() { _ = blobCache.Close() }()

	coalescer, err := fetch.New(fetch.Config{
		Source:  blobstore.BlobSource{Store: store},
		Store:   blobCache,
		TempDir: blobCache.TempDir(),
		Timeout: fetchTimeout,
	})
	if err != nil {
		return err
	}

	selector, err := cache.NewSelector(cfg.Cache.Pinned, cfg.Cache.Prefetch)
	if err != nil {
		return err
	}

	routes := make([]server.Route, len(cfg.Repos))
	for i, repo := range cfg.Repos {
		routes[i] = server.Route{Prefix: repo.Prefix, Repo: repo.Repo}
	}

	edge, err := server.New(server.Config{
		Store:               store,
		Cache:               blobCache,
		Coalescer:           coalescer,
		Selector:            selector,
		Routes:              routes,
		PollInterval:        time.Duration(cfg.PollInterval),
		WindowSize:          cfg.Window,
		PrefetchConcurrency: cfg.PrefetchConcurrency,
		Logger:              log,
	})
	if err != nil {
		return err
	}
	defer func() { _ = edge.Close() }()

	// The initial load is synchronous: a pinned set over its cap, or a
	// manifest that will not parse, must stop the edge here rather than after
	// it has started answering.
	if err := edge.Start(ctx); err != nil {
		return err
	}

	front := &http.Server{
		Addr:              cfg.Listen,
		Handler:           edge.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		// No WriteTimeout on purpose. A follower waiting on a 90 MiB download
		// legitimately holds a response open for minutes, and a write deadline
		// would cut it off mid-body.
	}
	admin := &http.Server{
		Addr:              cfg.AdminListen,
		Handler:           edge.AdminHandler(),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}

	log.Info("aquifer serving",
		"listen", cfg.Listen, "admin", cfg.AdminListen,
		"repos", len(cfg.Repos), "cache_dir", cfg.Cache.Dir,
		"max_size", int64(cfg.Cache.MaxSize), "version", version)
	_, _ = fmt.Fprintf(stdout, "aquifer %s serving %d repo(s) on %s (admin on %s)\n",
		version, len(cfg.Repos), cfg.Listen, cfg.AdminListen)

	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, srv := range []*http.Server{front, admin} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- fmt.Errorf("listen on %s: %w", srv.Addr, err)
			}
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case runErr = <-errs:
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	for _, srv := range []*http.Server{front, admin} {
		if err := srv.Shutdown(shutdownCtx); err != nil && runErr == nil {
			runErr = err
		}
	}
	wg.Wait()

	// Downloads run detached from the requests that started them, so stopping
	// the listeners does not stop them. Draining them keeps the wind-down
	// ordered rather than leaving goroutines writing into a cache the process
	// is about to abandon.
	if err := coalescer.Wait(shutdownCtx); err != nil {
		log.Warn("gave up waiting for in-flight downloads", "error", err)
	}
	return runErr
}

func overrideString(dest *string, value string) {
	if value != "" {
		*dest = value
	}
}

func parseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
