package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"github.com/nledez/aquifer/internal/publish"
)

// newLogger returns a structured logger, JSON in production and text when a
// human is watching.
func newLogger(w io.Writer, jsonOutput bool, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if jsonOutput {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

func runPublish(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr, "Usage: aquifer publish [flags] <publication-directory>\n\n"+
			"Uploads an aptly publication to object storage and commits a new revision.\n\n")
		fs.PrintDefaults()
	}

	var store storeFlags
	store.register(fs)
	repo := fs.String("repo", "", "repo name in object storage (required)")
	prefix := fs.String("prefix", "",
		"serving path prefix; defaults to the repo name, empty string with -prefix=/ serves at the root")
	concurrency := fs.Int("concurrency", 0, "parallel uploads (default: GOMAXPROCS, capped at 8)")
	jsonLogs := fs.Bool("json", false, "emit structured JSON logs")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	if *repo == "" {
		_, _ = fmt.Fprintln(stderr, "aquifer publish: -repo is required")
		return 2
	}

	servingPrefix := *prefix
	if !isSet(fs, "prefix") {
		servingPrefix = *repo
	}
	if servingPrefix == "/" {
		servingPrefix = ""
	}

	s, err := store.open()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "aquifer publish: %v\n", err)
		return 1
	}

	log := newLogger(stderr, *jsonLogs, slog.LevelInfo)
	res, err := publish.Run(ctx, publish.Publication{
		Dir:    fs.Arg(0),
		Repo:   *repo,
		Prefix: servingPrefix,
	}, publish.Options{
		Store:       s,
		Concurrency: *concurrency,
		Logger:      log,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "aquifer publish: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintf(stdout, "%s %s: %d entries, %s; uploaded %d blobs (%s), %d already present, %d hashed\n",
		res.Repo, res.Revision, res.Entries, humanBytes(res.Bytes),
		res.Uploaded, humanBytes(res.UploadedBytes), res.Skipped, res.Hashed)
	return 0
}

func runGC(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr, "Usage: aquifer gc [flags]\n\n"+
			"Deletes blobs no retained revision references, and manifests that have\n"+
			"fallen out of every repo's window.\n\n")
		fs.PrintDefaults()
	}

	var store storeFlags
	store.register(fs)
	keep := fs.Int("keep", 0, "revisions to retain per repo (default: 5)")
	grace := fs.Duration("grace", publish.DefaultGrace,
		"protect blobs written more recently than this, so a publication in flight survives")
	dryRun := fs.Bool("dry-run", false, "report what would be deleted without deleting it")
	jsonLogs := fs.Bool("json", false, "emit structured JSON logs")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	if *grace < 0 {
		_, _ = fmt.Fprintln(stderr, "aquifer gc: -grace cannot be negative")
		return 2
	}

	s, err := store.open()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "aquifer gc: %v\n", err)
		return 1
	}

	log := newLogger(stderr, *jsonLogs, slog.LevelInfo)
	res, err := publish.GC(ctx, publish.GCOptions{
		Store:  s,
		Keep:   *keep,
		Grace:  grace,
		DryRun: *dryRun,
		Logger: log,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "aquifer gc: %v\n", err)
		return 1
	}

	verb := "deleted"
	if *dryRun {
		verb = "would delete"
	}
	_, _ = fmt.Fprintf(stdout, "%d repos, %d blobs scanned, %d referenced; %s %d blobs (%s) and %d manifests; %d spared by the grace period\n",
		res.Repos, res.ScannedBlobs, res.ReferencedBlobs,
		verb, res.DeletedBlobs, humanBytes(res.DeletedBytes), res.DeletedManifests, res.KeptYoung)
	return 0
}

// isSet reports whether a flag was given explicitly, which is how an empty
// -prefix is told apart from an omitted one.
func isSet(fs *flag.FlagSet, name string) bool {
	var found bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
