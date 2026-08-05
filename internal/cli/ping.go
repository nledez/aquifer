package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// defaultPingTimeout keeps a health check from hanging. A health check that
// hangs is a broken health check.
const defaultPingTimeout = 2 * time.Second

// configSearchPath is where ping looks for a configuration file when it was
// not told where one is.
var configSearchPath = []string{
	"/etc/aquifer/config.yaml",
	"/etc/aquifer/aquifer.yaml",
	"aquifer.yaml",
}

// pingReport is the part of /readyz that ping reasons about. It is decoded
// loosely on purpose: ping must keep working against an instance running a
// slightly different version.
type pingReport struct {
	Ready  bool   `json:"ready"`
	Reason string `json:"reason"`
	Repos  []struct {
		Repo     string   `json:"repo"`
		Revision string   `json:"revision"`
		Expired  []string `json:"expired_suites"`
	} `json:"repos"`
	Cache struct {
		PinnedMissing int `json:"pinned_missing"`
	} `json:"cache"`
}

// runPing checks the local instance's readiness and exits 0 or 1.
//
// It deliberately depends on nothing else in the runtime: no manifest is
// loaded, no S3 connection is opened, no cache directory is touched. It has to
// work inside a distroless container that has no shell, with no arguments at
// all.
func runPing(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ping", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: aquifer ping [flags]\n\n"+
			"Queries /readyz on the local instance. Exits 0 when it is ready, 1 otherwise.\n"+
			"Silent on success; on failure it prints which condition failed.\n\n")
		fs.PrintDefaults()
	}

	addr := fs.String("addr", "", "admin address to query (env AQUIFER_ADMIN_ADDR, else the config file)")
	configPath := fs.String("config", "", "configuration file to read the admin address from")
	timeout := fs.Duration("timeout", defaultPingTimeout, "how long to wait before giving up")
	verbose := fs.Bool("verbose", false, "print the full /readyz document")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	var explicit []string
	if *addr != "" {
		explicit = append(explicit, *addr)
	}
	target := resolveAdminAddr(explicit, os.Getenv, findConfig(*configPath))

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	body, status, err := fetchReadyz(ctx, target)
	if err != nil {
		fmt.Fprintf(stderr, "aquifer: %s is not answering: %v\n", target, err)
		return 1
	}

	if *verbose {
		fmt.Fprintln(stdout, strings.TrimRight(string(body), "\n"))
	}

	var report pingReport
	if err := json.Unmarshal(body, &report); err != nil {
		fmt.Fprintf(stderr, "aquifer: %s returned status %d with an unreadable body: %v\n",
			target, status, err)
		return 1
	}

	if status == http.StatusOK && report.Ready {
		return 0
	}
	fmt.Fprintf(stderr, "aquifer: not ready: %s\n", describeFailure(report, status))
	return 1
}

// describeFailure turns a readiness document into the single line an operator
// reads out of docker inspect.
func describeFailure(report pingReport, status int) string {
	if report.Reason != "" {
		return report.Reason
	}

	var reasons []string
	if report.Cache.PinnedMissing > 0 {
		reasons = append(reasons, fmt.Sprintf("%d pinned blob(s) not yet on disk", report.Cache.PinnedMissing))
	}
	for _, repo := range report.Repos {
		if repo.Revision == "" {
			reasons = append(reasons, repo.Repo+" has no revision loaded")
		}
		for _, suite := range repo.Expired {
			reasons = append(reasons, repo.Repo+" suite "+suite+" is past its Valid-Until")
		}
	}
	if len(reasons) == 0 {
		return fmt.Sprintf("readyz answered %d without saying why", status)
	}
	return strings.Join(reasons, "; ")
}

func fetchReadyz(ctx context.Context, target string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(target, "/")+"/readyz", nil)
	if err != nil {
		return nil, 0, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	// The document is small; a cap keeps a confused endpoint from filling
	// memory in a health check.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// resolveAdminAddr picks the admin address: explicit flags first, then the
// environment, then a readable configuration file, then a local default.
func resolveAdminAddr(explicit []string, getenv func(string) string, configPath string) string {
	for _, candidate := range explicit {
		if candidate != "" {
			return normaliseAdminURL(candidate)
		}
	}
	if v := getenv("AQUIFER_ADMIN_ADDR"); v != "" {
		return normaliseAdminURL(v)
	}
	if configPath != "" {
		if cfg, err := LoadConfig(configPath); err == nil && cfg.AdminListen != "" {
			return normaliseAdminURL(cfg.AdminListen)
		}
	}
	return normaliseAdminURL(DefaultAdminListen)
}

// normaliseAdminURL turns a bind address into something dialable. A wildcard
// bind is not a usable target, so it becomes loopback: ping only ever talks to
// the instance sharing its container.
func normaliseAdminURL(addr string) string {
	if !strings.Contains(addr, "://") {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return "http://" + addr
		}
		switch host {
		case "", "0.0.0.0", "::", "[::]":
			host = "127.0.0.1"
		}
		addr = net.JoinHostPort(host, port)
		return "http://" + addr
	}
	return strings.TrimSuffix(addr, "/")
}

// findConfig returns the configuration file to consult, if any.
func findConfig(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := os.Getenv("AQUIFER_CONFIG"); v != "" {
		return v
	}
	for _, candidate := range configSearchPath {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}
