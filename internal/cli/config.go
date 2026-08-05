package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"go.yaml.in/yaml/v3"
)

// Defaults an operator can leave alone.
const (
	DefaultListen      = "0.0.0.0:8080"
	DefaultAdminListen = "127.0.0.1:8081"

	defaultPollInterval        = 15 * time.Second
	defaultWindow              = 5
	defaultPrefetchConcurrency = 4
)

// Config is the whole edge configuration.
//
// SPEC section 9: the file is the base, the environment overrides it, and
// flags override that. Every layer is explicit rather than reflected over, so
// that a key can be renamed without silently losing its override.
type Config struct {
	Listen      string      `yaml:"listen"`
	AdminListen string      `yaml:"admin_listen"`
	Log         LogConfig   `yaml:"log"`
	S3          S3Config    `yaml:"s3"`
	Cache       CacheConfig `yaml:"cache"`

	PollInterval        Duration `yaml:"poll_interval"`
	Window              int      `yaml:"window"`
	PrefetchConcurrency int      `yaml:"prefetch_concurrency"`

	Repos []RepoConfig `yaml:"repos"`
}

// LogConfig controls structured logging.
type LogConfig struct {
	// Format is "json" or "text". JSON is the default, since production is
	// where this runs.
	Format string `yaml:"format"`
	Level  string `yaml:"level"`
}

// S3Config points at object storage.
type S3Config struct {
	Endpoint  string `yaml:"endpoint"`
	Bucket    string `yaml:"bucket"`
	Prefix    string `yaml:"prefix"`
	Region    string `yaml:"region"`
	PathStyle bool   `yaml:"path_style"`
	Insecure  bool   `yaml:"insecure"`
	// Credentials belong in the environment, not in a file that ends up in a
	// git repository. They are accepted here for completeness only.
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
}

// CacheConfig is the operator's main tuning lever.
type CacheConfig struct {
	Dir           string `yaml:"dir"`
	MaxSize       Bytes  `yaml:"max_size"`
	PinnedMaxSize Bytes  `yaml:"pinned_max_size"`
	TempReserve   Bytes  `yaml:"temp_reserve"`

	Pinned   []string `yaml:"pinned"`
	Prefetch []string `yaml:"prefetch"`
}

// RepoConfig maps a published repo to the path it is served under.
type RepoConfig struct {
	Repo string `yaml:"repo"`
	// Prefix may be empty, which serves the publication at the archive root.
	Prefix string `yaml:"prefix"`
}

// DefaultConfig returns the configuration before any file is read.
func DefaultConfig() *Config {
	return &Config{
		Listen:      DefaultListen,
		AdminListen: DefaultAdminListen,
		Log:         LogConfig{Format: "json", Level: "info"},
		S3:          S3Config{PathStyle: true},
		Cache: CacheConfig{
			Pinned:   []string{"**/dists/**", "dists/**"},
			Prefetch: []string{"**/dists/**", "dists/**"},
		},
		PollInterval:        Duration(defaultPollInterval),
		Window:              defaultWindow,
		PrefetchConcurrency: defaultPrefetchConcurrency,
	}
}

// LoadConfig reads a configuration file over the defaults.
func LoadConfig(path string) (*Config, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	cfg := DefaultConfig()
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	// An unknown key is almost always a typo, and a typo that silently takes
	// the default is the kind of thing found months later, in an incident.
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return cfg, nil
}

// ApplyEnv overlays environment variables on the configuration.
func (c *Config) ApplyEnv(getenv func(string) string) error {
	str := func(name string, dest *string) {
		if v := getenv(name); v != "" {
			*dest = v
		}
	}
	str("AQUIFER_LISTEN", &c.Listen)
	str("AQUIFER_ADMIN_LISTEN", &c.AdminListen)
	str("AQUIFER_LOG_FORMAT", &c.Log.Format)
	str("AQUIFER_LOG_LEVEL", &c.Log.Level)
	str("AQUIFER_S3_ENDPOINT", &c.S3.Endpoint)
	str("AQUIFER_S3_BUCKET", &c.S3.Bucket)
	str("AQUIFER_S3_PREFIX", &c.S3.Prefix)
	str("AQUIFER_S3_REGION", &c.S3.Region)
	str("AQUIFER_CACHE_DIR", &c.Cache.Dir)

	// Credentials are read from the environment first, and from the standard
	// AWS names too, so that existing tooling works unchanged.
	if v := firstOf(getenv, "AQUIFER_S3_ACCESS_KEY", "AWS_ACCESS_KEY_ID"); v != "" {
		c.S3.AccessKey = v
	}
	if v := firstOf(getenv, "AQUIFER_S3_SECRET_KEY", "AWS_SECRET_ACCESS_KEY"); v != "" {
		c.S3.SecretKey = v
	}

	if v := getenv("AQUIFER_S3_PATH_STYLE"); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("config: AQUIFER_S3_PATH_STYLE: %w", err)
		}
		c.S3.PathStyle = parsed
	}
	if v := getenv("AQUIFER_S3_INSECURE"); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("config: AQUIFER_S3_INSECURE: %w", err)
		}
		c.S3.Insecure = parsed
	}

	for _, size := range []struct {
		name string
		dest *Bytes
	}{
		{"AQUIFER_CACHE_MAX_SIZE", &c.Cache.MaxSize},
		{"AQUIFER_CACHE_PINNED_MAX_SIZE", &c.Cache.PinnedMaxSize},
		{"AQUIFER_CACHE_TEMP_RESERVE", &c.Cache.TempReserve},
	} {
		v := getenv(size.name)
		if v == "" {
			continue
		}
		parsed, err := ParseBytes(v)
		if err != nil {
			return fmt.Errorf("config: %s: %w", size.name, err)
		}
		*size.dest = Bytes(parsed)
	}

	if v := getenv("AQUIFER_POLL_INTERVAL"); v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("config: AQUIFER_POLL_INTERVAL: %w", err)
		}
		c.PollInterval = Duration(parsed)
	}
	for _, count := range []struct {
		name string
		dest *int
	}{
		{"AQUIFER_WINDOW", &c.Window},
		{"AQUIFER_PREFETCH_CONCURRENCY", &c.PrefetchConcurrency},
	} {
		v := getenv(count.name)
		if v == "" {
			continue
		}
		parsed, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: %s: %w", count.name, err)
		}
		*count.dest = parsed
	}
	return nil
}

func firstOf(getenv func(string) string, names ...string) string {
	for _, name := range names {
		if v := getenv(name); v != "" {
			return v
		}
	}
	return ""
}

// Validate reports everything wrong with the configuration before anything is
// opened, so that a bad deployment fails at startup rather than on the first
// request.
func (c *Config) Validate() error {
	var problems []string

	if c.Listen == "" {
		problems = append(problems, "listen is required")
	}
	if c.AdminListen == "" {
		problems = append(problems, "admin_listen is required")
	}
	if c.Listen != "" && c.Listen == c.AdminListen {
		problems = append(problems,
			"listen and admin_listen must differ; metrics and probes belong on a separate port")
	}
	if c.Cache.Dir == "" {
		problems = append(problems, "cache.dir is required")
	}
	if c.Cache.MaxSize <= 0 {
		problems = append(problems, "cache.max_size must be positive")
	}
	if c.Cache.PinnedMaxSize < 0 {
		problems = append(problems, "cache.pinned_max_size cannot be negative")
	}
	if c.Cache.TempReserve < 0 {
		problems = append(problems, "cache.temp_reserve cannot be negative")
	}
	if c.S3.Endpoint == "" {
		problems = append(problems, "s3.endpoint is required")
	}
	if c.S3.Bucket == "" {
		problems = append(problems, "s3.bucket is required")
	}
	if c.Window <= 0 {
		problems = append(problems, "window must be positive")
	}
	if time.Duration(c.PollInterval) <= 0 {
		problems = append(problems, "poll_interval must be positive")
	}

	for _, group := range []struct {
		name     string
		patterns []string
	}{{"cache.pinned", c.Cache.Pinned}, {"cache.prefetch", c.Cache.Prefetch}} {
		for _, pattern := range group.patterns {
			if !doublestar.ValidatePattern(pattern) {
				problems = append(problems, fmt.Sprintf("%s: %q is not a valid glob", group.name, pattern))
			}
		}
	}

	if len(c.Repos) == 0 {
		problems = append(problems, "at least one entry under repos is required")
	}
	seen := map[string]string{}
	for i, repo := range c.Repos {
		if repo.Repo == "" {
			problems = append(problems, fmt.Sprintf("repos[%d]: repo is required", i))
			continue
		}
		prefix := strings.Trim(repo.Prefix, "/")
		if other, dup := seen[prefix]; dup {
			problems = append(problems, fmt.Sprintf(
				"repos: prefix %q is claimed by both %q and %q", prefix, other, repo.Repo))
			continue
		}
		seen[prefix] = repo.Repo
	}

	if len(problems) > 0 {
		return fmt.Errorf("config: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Bytes is a size that reads as "5GiB" in YAML and as a plain byte count in
// code.
type Bytes int64

// UnmarshalYAML accepts either a bare integer or a suffixed size.
func (b *Bytes) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return err
	}
	parsed, err := ParseBytes(raw)
	if err != nil {
		return err
	}
	*b = Bytes(parsed)
	return nil
}

// ParseBytes reads a size such as "5GiB", "2MB" or a bare byte count.
//
// A bare number is bytes. Guessing a larger unit would silently size a cache
// a million times wrong in the direction that looks fine until the disk fills.
func ParseBytes(s string) (int64, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, errors.New("empty size")
	}

	digits := 0
	for digits < len(trimmed) && trimmed[digits] >= '0' && trimmed[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return 0, fmt.Errorf("size %q does not start with a number", s)
	}

	value, err := strconv.ParseInt(trimmed[:digits], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}

	unit := strings.ToLower(strings.TrimSpace(trimmed[digits:]))
	multiplier, ok := map[string]int64{
		"":    1,
		"b":   1,
		"kib": 1 << 10,
		"mib": 1 << 20,
		"gib": 1 << 30,
		"tib": 1 << 40,
		"kb":  1_000,
		"mb":  1_000_000,
		"gb":  1_000_000_000,
		"tb":  1_000_000_000_000,
	}[unit]
	if !ok {
		return 0, fmt.Errorf("size %q: unknown unit %q", s, unit)
	}
	return value * multiplier, nil
}

// Duration reads as "30s" in YAML.
type Duration time.Duration

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("duration %q: %w", raw, err)
	}
	*d = Duration(parsed)
	return nil
}
