package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aquifer.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const fullConfig = `
listen: "0.0.0.0:8080"
admin_listen: "127.0.0.1:8081"

log:
  format: json
  level: debug

s3:
  endpoint: https://s3.example.net
  bucket: aquifer
  prefix: mirror
  region: gra
  path_style: true

poll_interval: 30s
window: 7
prefetch_concurrency: 6

cache:
  dir: /var/cache/aquifer
  max_size: 5GiB
  pinned_max_size: 1GiB
  temp_reserve: 3GiB
  pinned:
    - "**/dists/**"
    - "dists/**"
  prefetch:
    - "**/dists/**"

repos:
  - repo: debian/bookworm
    prefix: debian/bookworm
  - repo: root
    prefix: ""
`

func TestLoadConfigReadsEveryField(t *testing.T) {
	t.Parallel()

	cfg, err := LoadConfig(writeConfig(t, fullConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Listen != "0.0.0.0:8080" || cfg.AdminListen != "127.0.0.1:8081" {
		t.Fatalf("listen addresses = %q / %q", cfg.Listen, cfg.AdminListen)
	}
	if cfg.Log.Format != "json" || cfg.Log.Level != "debug" {
		t.Fatalf("log = %+v", cfg.Log)
	}
	if cfg.S3.Endpoint != "https://s3.example.net" || cfg.S3.Bucket != "aquifer" ||
		cfg.S3.Prefix != "mirror" || cfg.S3.Region != "gra" || !cfg.S3.PathStyle {
		t.Fatalf("s3 = %+v", cfg.S3)
	}
	if time.Duration(cfg.PollInterval) != 30*time.Second {
		t.Fatalf("poll_interval = %v", time.Duration(cfg.PollInterval))
	}
	if cfg.Window != 7 || cfg.PrefetchConcurrency != 6 {
		t.Fatalf("window = %d, prefetch = %d", cfg.Window, cfg.PrefetchConcurrency)
	}

	if cfg.Cache.Dir != "/var/cache/aquifer" {
		t.Fatalf("cache dir = %q", cfg.Cache.Dir)
	}
	if int64(cfg.Cache.MaxSize) != 5<<30 {
		t.Fatalf("max_size = %d, want %d", cfg.Cache.MaxSize, int64(5)<<30)
	}
	if int64(cfg.Cache.PinnedMaxSize) != 1<<30 || int64(cfg.Cache.TempReserve) != 3<<30 {
		t.Fatalf("pinned_max_size = %d, temp_reserve = %d", cfg.Cache.PinnedMaxSize, cfg.Cache.TempReserve)
	}
	if !slices.Equal(cfg.Cache.Pinned, []string{"**/dists/**", "dists/**"}) {
		t.Fatalf("pinned = %v", cfg.Cache.Pinned)
	}

	if len(cfg.Repos) != 2 {
		t.Fatalf("repos = %+v", cfg.Repos)
	}
	if cfg.Repos[1].Repo != "root" || cfg.Repos[1].Prefix != "" {
		t.Fatalf("the root publication was not read: %+v", cfg.Repos[1])
	}
}

func TestParseBytesUnderstandsTheUnitsTheSpecUses(t *testing.T) {
	t.Parallel()

	cases := map[string]int64{
		"0":     0,
		"512":   512,
		"1KiB":  1024,
		"5GiB":  5 << 30,
		"3 GiB": 3 << 30,
		"1TiB":  1 << 40,
		"1kb":   1000,
		"2MB":   2_000_000,
		"10B":   10,
	}
	for in, want := range cases {
		got, err := ParseBytes(in)
		if err != nil {
			t.Fatalf("ParseBytes(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseBytes(%q) = %d, want %d", in, got, want)
		}
	}

	for _, bad := range []string{"", "GiB", "-1GiB", "5 gigabytes", "1.5GiB", "0x10"} {
		if _, err := ParseBytes(bad); err == nil {
			t.Fatalf("ParseBytes(%q) accepted junk", bad)
		}
	}
}

// A cache budget written as a bare number is bytes, not megabytes. Getting
// that wrong silently would size a cache a million times too small.
func TestSizesAcceptBareIntegersAsBytes(t *testing.T) {
	t.Parallel()

	cfg, err := LoadConfig(writeConfig(t, "cache:\n  dir: /tmp/x\n  max_size: 1048576\n"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if int64(cfg.Cache.MaxSize) != 1048576 {
		t.Fatalf("max_size = %d", cfg.Cache.MaxSize)
	}
}

func TestLoadConfigAppliesDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := LoadConfig(writeConfig(t, "cache:\n  dir: /tmp/x\n"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Listen != DefaultListen || cfg.AdminListen != DefaultAdminListen {
		t.Fatalf("listen defaults = %q / %q", cfg.Listen, cfg.AdminListen)
	}
	if time.Duration(cfg.PollInterval) != 15*time.Second {
		t.Fatalf("poll_interval default = %v", time.Duration(cfg.PollInterval))
	}
	if cfg.Window != 5 {
		t.Fatalf("window default = %d, want 5", cfg.Window)
	}
	if cfg.Log.Format != "json" {
		t.Fatalf("log format default = %q, want json", cfg.Log.Format)
	}
}

func TestMissingConfigFileIsAnError(t *testing.T) {
	t.Parallel()

	if _, err := LoadConfig(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("LoadConfig accepted a path that does not exist")
	}
}

func TestLoadConfigRejectsUnknownKeys(t *testing.T) {
	t.Parallel()

	_, err := LoadConfig(writeConfig(t, "cache:\n  dir: /tmp/x\n  max_sise: 5GiB\n"))
	if err == nil {
		t.Fatal("LoadConfig accepted a misspelled key; a typo must not silently take a default")
	}
}

// SPEC section 9: the file is overridden by environment variables, which are
// in turn overridden by flags.
func TestEnvironmentOverridesTheFile(t *testing.T) {
	t.Parallel()

	cfg, err := LoadConfig(writeConfig(t, fullConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	env := map[string]string{
		"AQUIFER_LISTEN":         "0.0.0.0:9999",
		"AQUIFER_S3_BUCKET":      "other-bucket",
		"AQUIFER_S3_ACCESS_KEY":  "key",
		"AQUIFER_S3_SECRET_KEY":  "secret",
		"AQUIFER_CACHE_MAX_SIZE": "2GiB",
		"AQUIFER_POLL_INTERVAL":  "5s",
	}
	if err := cfg.ApplyEnv(func(k string) string { return env[k] }); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}

	if cfg.Listen != "0.0.0.0:9999" {
		t.Fatalf("listen = %q", cfg.Listen)
	}
	if cfg.S3.Bucket != "other-bucket" || cfg.S3.AccessKey != "key" || cfg.S3.SecretKey != "secret" {
		t.Fatalf("s3 = %+v", cfg.S3)
	}
	if int64(cfg.Cache.MaxSize) != 2<<30 {
		t.Fatalf("max_size = %d", cfg.Cache.MaxSize)
	}
	if time.Duration(cfg.PollInterval) != 5*time.Second {
		t.Fatalf("poll_interval = %v", time.Duration(cfg.PollInterval))
	}
}

func TestApplyEnvRejectsAMalformedValue(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	err := cfg.ApplyEnv(func(k string) string {
		if k == "AQUIFER_CACHE_MAX_SIZE" {
			return "five gigabytes"
		}
		return ""
	})
	if err == nil {
		t.Fatal("ApplyEnv accepted a malformed size instead of reporting it")
	}
}

func TestValidateCatchesAnUnusableConfiguration(t *testing.T) {
	t.Parallel()

	base := func() *Config {
		cfg := DefaultConfig()
		cfg.Cache.Dir = "/var/cache/aquifer"
		cfg.Cache.MaxSize = 1 << 20
		cfg.S3.Endpoint = "https://s3.example.net"
		cfg.S3.Bucket = "aquifer"
		cfg.Repos = []RepoConfig{{Repo: "debian/bookworm", Prefix: "debian/bookworm"}}
		return cfg
	}

	if err := base().Validate(); err != nil {
		t.Fatalf("a complete configuration was rejected: %v", err)
	}

	cases := map[string]func(*Config){
		"no cache dir":     func(c *Config) { c.Cache.Dir = "" },
		"no cache budget":  func(c *Config) { c.Cache.MaxSize = 0 },
		"no bucket":        func(c *Config) { c.S3.Bucket = "" },
		"no endpoint":      func(c *Config) { c.S3.Endpoint = "" },
		"no repos":         func(c *Config) { c.Repos = nil },
		"repo without id":  func(c *Config) { c.Repos = []RepoConfig{{Prefix: "x"}} },
		"duplicate prefix": func(c *Config) { c.Repos = append(c.Repos, RepoConfig{Repo: "other", Prefix: "debian/bookworm"}) },
		"bad glob":         func(c *Config) { c.Cache.Pinned = []string{"["} },
		"no listen":        func(c *Config) { c.Listen = "" },
		"same port twice":  func(c *Config) { c.AdminListen = c.Listen },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := base()
			mutate(cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate accepted a configuration with %s", name)
			}
		})
	}
}

// A repo entry with no prefix key is the root publication, which is a
// legitimate and load-bearing case.
func TestARepoMayServeAtTheRoot(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.Cache.Dir = "/tmp/x"
	cfg.Cache.MaxSize = 1 << 20
	cfg.S3.Endpoint = "https://s3.example.net"
	cfg.S3.Bucket = "aquifer"
	cfg.Repos = []RepoConfig{
		{Repo: "debian/bookworm", Prefix: "debian/bookworm"},
		{Repo: "root", Prefix: ""},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected a root publication: %v", err)
	}
}
