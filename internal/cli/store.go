package cli

import (
	"flag"
	"os"

	"github.com/nledez/aquifer/internal/blobstore"
)

// storeFlags collects the object storage settings shared by the master
// subcommands. Every one of them falls back to an environment variable so that
// credentials never have to appear in a process listing.
type storeFlags struct {
	endpoint  string
	bucket    string
	keyPrefix string
	region    string
	pathStyle bool
	insecure  bool
}

func (s *storeFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&s.endpoint, "endpoint", env("AQUIFER_S3_ENDPOINT", ""),
		"object storage endpoint URL (env AQUIFER_S3_ENDPOINT)")
	fs.StringVar(&s.bucket, "bucket", env("AQUIFER_S3_BUCKET", ""),
		"bucket name (env AQUIFER_S3_BUCKET)")
	fs.StringVar(&s.keyPrefix, "key-prefix", env("AQUIFER_S3_PREFIX", ""),
		"key prefix inside the bucket (env AQUIFER_S3_PREFIX)")
	fs.StringVar(&s.region, "region", env("AQUIFER_S3_REGION", ""),
		"region, if the endpoint needs one (env AQUIFER_S3_REGION)")
	fs.BoolVar(&s.pathStyle, "path-style", envBool("AQUIFER_S3_PATH_STYLE", true),
		"use bucket-in-path addressing, which most non-AWS endpoints need")
	fs.BoolVar(&s.insecure, "insecure", envBool("AQUIFER_S3_INSECURE", false),
		"allow plain HTTP for a bare host:port endpoint")
}

func (s *storeFlags) open() (blobstore.Store, error) {
	return blobstore.NewS3(blobstore.Config{
		Endpoint:  s.endpoint,
		Bucket:    s.bucket,
		Prefix:    s.keyPrefix,
		Region:    s.region,
		PathStyle: s.pathStyle,
		Insecure:  s.insecure,
		AccessKey: firstEnv("AQUIFER_S3_ACCESS_KEY", "AWS_ACCESS_KEY_ID"),
		SecretKey: firstEnv("AQUIFER_S3_SECRET_KEY", "AWS_SECRET_ACCESS_KEY"),
	})
}

func env(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return fallback
}

func envBool(name string, fallback bool) bool {
	switch os.Getenv(name) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}
