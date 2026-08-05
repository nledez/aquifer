package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readyzServer stands in for a local instance's admin port.
func readyzServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const readyBody = `{"ready":true,"repos":[{"repo":"debian/bookworm","revision":"1754400000-aaaa","age_seconds":12}],
"cache":{"bytes":1024,"objects":3,"pinned_bytes":512,"pinned_objects":2,"pinned_missing":0}}`

const notReadyBody = `{"ready":false,"reason":"debian/bookworm suite bookworm is past its Valid-Until",
"repos":[{"repo":"debian/bookworm","revision":"1754400000-aaaa","expired_suites":["bookworm"]}],
"cache":{"bytes":1024,"objects":3,"pinned_bytes":512,"pinned_objects":2,"pinned_missing":0}}`

// SPEC section 10: silent on success, exit 0.
func TestPingIsSilentWhenReady(t *testing.T) {
	t.Parallel()

	srv := readyzServer(t, http.StatusOK, readyBody)
	var stdout, stderr bytes.Buffer

	code := runPing(context.Background(), []string{"--addr", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("ping was not silent: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// SPEC section 10: on failure, one line on stderr saying which condition
// failed. That is what gets read from docker inspect at 3am.
func TestPingReportsWhichConditionFailed(t *testing.T) {
	t.Parallel()

	srv := readyzServer(t, http.StatusServiceUnavailable, notReadyBody)
	var stdout, stderr bytes.Buffer

	code := runPing(context.Background(), []string{"--addr", srv.URL}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	line := strings.TrimSpace(stderr.String())
	if strings.Count(line, "\n") != 0 {
		t.Fatalf("stderr should be one line, got:\n%s", line)
	}
	if !strings.Contains(line, "Valid-Until") {
		t.Fatalf("stderr does not name the failing condition: %q", line)
	}
}

// A readyz that is down at all is also a failure, and must say so rather than
// hanging or panicking.
func TestPingReportsAnUnreachableInstance(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runPing(context.Background(), []string{"--addr", "http://127.0.0.1:1", "--timeout", "200ms"},
		&stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if stderr.Len() == 0 {
		t.Fatal("an unreachable instance produced no diagnostic")
	}
}

func TestPingVerbosePrintsTheReadyzDetail(t *testing.T) {
	t.Parallel()

	srv := readyzServer(t, http.StatusOK, readyBody)
	var stdout, stderr bytes.Buffer

	code := runPing(context.Background(), []string{"--addr", srv.URL, "--verbose"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	out := stdout.String()
	for _, want := range []string{"debian/bookworm", "1754400000-aaaa", "pinned_objects"} {
		if !strings.Contains(out, want) {
			t.Fatalf("verbose output is missing %q:\n%s", want, out)
		}
	}
}

// SPEC section 10: the address is resolved from the flag, then the
// environment, then the config file, then a local default. It has to work with
// no arguments at all inside a container.
func TestPingResolvesItsAddressInOrder(t *testing.T) {
	t.Parallel()

	srv := readyzServer(t, http.StatusOK, readyBody)

	t.Run("flag wins over environment", func(t *testing.T) {
		t.Parallel()
		got := resolveAdminAddr([]string{srv.URL}, func(string) string { return "http://from-env:1" }, "")
		if got != srv.URL {
			t.Fatalf("addr = %q, want the flag value", got)
		}
	})

	t.Run("environment wins over the config file", func(t *testing.T) {
		t.Parallel()
		path := writeConfig(t, "admin_listen: 127.0.0.1:7777\ncache:\n  dir: /tmp/x\n")
		got := resolveAdminAddr(nil, func(k string) string {
			if k == "AQUIFER_ADMIN_ADDR" {
				return "http://from-env:1234"
			}
			return ""
		}, path)
		if got != "http://from-env:1234" {
			t.Fatalf("addr = %q, want the environment value", got)
		}
	})

	t.Run("the config file is used when it is readable", func(t *testing.T) {
		t.Parallel()
		path := writeConfig(t, "admin_listen: 127.0.0.1:7777\ncache:\n  dir: /tmp/x\n")
		got := resolveAdminAddr(nil, func(string) string { return "" }, path)
		if got != "http://127.0.0.1:7777" {
			t.Fatalf("addr = %q, want the configured admin address", got)
		}
	})

	t.Run("a local default is used when nothing else is available", func(t *testing.T) {
		t.Parallel()
		got := resolveAdminAddr(nil, func(string) string { return "" },
			filepath.Join(t.TempDir(), "absent.yaml"))
		if got != "http://"+DefaultAdminListen {
			t.Fatalf("addr = %q, want the built-in default", got)
		}
	})

	t.Run("a wildcard bind address becomes loopback", func(t *testing.T) {
		t.Parallel()
		path := writeConfig(t, "admin_listen: 0.0.0.0:7777\ncache:\n  dir: /tmp/x\n")
		got := resolveAdminAddr(nil, func(string) string { return "" }, path)
		if got != "http://127.0.0.1:7777" {
			t.Fatalf("addr = %q; a wildcard bind is not a usable target", got)
		}
	})
}

// The whole point of ping is to run in a distroless container with no shell
// and no arguments, so it must not need anything the runtime needs.
func TestPingNeedsNoRuntimeState(t *testing.T) {
	t.Parallel()

	srv := readyzServer(t, http.StatusOK, readyBody)

	// No cache directory, no S3 credentials, no config file: none of it is
	// touched.
	dir := t.TempDir()
	if err := os.Chdir(dir); err == nil {
		t.Cleanup(func() { _ = os.Chdir(dir) })
	}

	var stdout, stderr bytes.Buffer
	if code := runPing(context.Background(), []string{"--addr", srv.URL}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
}
