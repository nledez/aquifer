//go:build integration

// These tests run the same contract as the in-memory store, but against a real
// S3 implementation. That is the only way to find out whether the conditional
// GET, the path-style addressing and the listing pagination actually behave.
//
//	go test -tags=integration ./internal/blobstore/...
package blobstore_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/nledez/aquifer/internal/blobstore"
	"github.com/nledez/aquifer/internal/blobstore/blobstoretest"
)

const (
	defaultImage = "minio/minio:RELEASE.2025-04-22T22-12-26Z"
	accessKey    = "aquifer-test"
	secretKey    = "aquifer-test-secret"
)

var bucketCounter atomic.Int64

// minioEndpoint starts a MinIO container and returns its host:port.
func minioEndpoint(t *testing.T) string {
	t.Helper()

	if endpoint := os.Getenv("AQUIFER_TEST_S3_ENDPOINT"); endpoint != "" {
		return endpoint
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not available; set AQUIFER_TEST_S3_ENDPOINT to test against an existing store")
	}

	image := os.Getenv("AQUIFER_TEST_MINIO_IMAGE")
	if image == "" {
		image = defaultImage
	}

	out, err := exec.Command("docker", "run", "-d", "--rm",
		"-p", "127.0.0.1:0:9000",
		"-e", "MINIO_ROOT_USER="+accessKey,
		"-e", "MINIO_ROOT_PASSWORD="+secretKey,
		image, "server", "/data",
	).Output()
	if err != nil {
		t.Skipf("cannot start %s: %v", image, err)
	}
	container := strings.TrimSpace(string(out))
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", container).Run()
	})

	portOut, err := exec.Command("docker", "port", container, "9000/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	endpoint := strings.TrimSpace(strings.SplitN(string(portOut), "\n", 2)[0])

	waitHealthy(t, endpoint)
	return endpoint
}

func waitHealthy(t *testing.T, endpoint string) {
	t.Helper()

	deadline := time.Now().Add(60 * time.Second)
	url := "http://" + endpoint + "/minio/health/live"
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("build health request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("MinIO at %s never became healthy", endpoint)
}

// newBucket creates an empty bucket and returns its name.
func newBucket(t *testing.T, endpoint string) string {
	t.Helper()

	client, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure:       false,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatalf("minio client: %v", err)
	}
	name := fmt.Sprintf("aquifer-%d-%s", bucketCounter.Add(1), rand.Text()[:8])
	name = strings.ToLower(name)
	if err := client.MakeBucket(t.Context(), name, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("MakeBucket %s: %v", name, err)
	}
	return name
}

func newS3(t *testing.T, endpoint, bucket, prefix string) blobstore.Store {
	t.Helper()

	s, err := blobstore.NewS3(blobstore.Config{
		Endpoint:  "http://" + endpoint,
		Bucket:    bucket,
		Prefix:    prefix,
		AccessKey: accessKey,
		SecretKey: secretKey,
		PathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	return s
}

func TestS3StoreSatisfiesTheContract(t *testing.T) {
	endpoint := minioEndpoint(t)

	blobstoretest.RunStoreContract(t, func(t *testing.T) blobstore.Store {
		return newS3(t, endpoint, newBucket(t, endpoint), "mirror")
	})
}

// Two deployments sharing a bucket must not see each other's objects.
func TestS3PrefixesAreIsolated(t *testing.T) {
	endpoint := minioEndpoint(t)
	bucket := newBucket(t, endpoint)
	ctx := t.Context()

	first := newS3(t, endpoint, bucket, "one")
	second := newS3(t, endpoint, bucket, "two")

	body := []byte("only in the first deployment")
	hash := blobstoretest.Hash(body)
	if err := first.PutBlob(ctx, hash, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if err := first.SetRef(ctx, "debian/bookworm", "1754400000-aaaa"); err != nil {
		t.Fatalf("SetRef: %v", err)
	}

	blobs, err := second.ListBlobs(ctx)
	if err != nil {
		t.Fatalf("ListBlobs: %v", err)
	}
	if len(blobs) != 0 {
		t.Fatalf("the second prefix sees %d blobs from the first", len(blobs))
	}
	repos, err := second.ListRepos(ctx)
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 0 {
		t.Fatalf("the second prefix sees %d repos from the first", len(repos))
	}
}

// A 90 MiB package is an ordinary object here, so nothing may buffer a blob
// whole. This also exercises the multipart path that small blobs never reach.
func TestS3StreamsLargeBlobsWithoutCorruption(t *testing.T) {
	endpoint := minioEndpoint(t)
	s := newS3(t, endpoint, newBucket(t, endpoint), "mirror")
	ctx := t.Context()

	const size = 24 << 20
	body := make([]byte, size)
	if _, err := rand.Read(body); err != nil {
		t.Fatalf("rand: %v", err)
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])

	if err := s.PutBlob(ctx, hash, bytes.NewReader(body), size); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}

	rc, err := s.GetBlob(ctx, hash)
	if err != nil {
		t.Fatalf("GetBlob: %v", err)
	}
	defer func() { _ = rc.Close() }()

	digest := sha256.New()
	n, err := io.Copy(digest, rc)
	if err != nil {
		t.Fatalf("stream blob: %v", err)
	}
	if n != size {
		t.Fatalf("read %d bytes, want %d", n, size)
	}
	if got := hex.EncodeToString(digest.Sum(nil)); got != hash {
		t.Fatalf("digest after round trip = %s, want %s", got, hash)
	}
}

// The 15s ref poll leans on If-None-Match; verify a real server honours it.
func TestS3ConditionalRefPollAgainstARealServer(t *testing.T) {
	endpoint := minioEndpoint(t)
	s := newS3(t, endpoint, newBucket(t, endpoint), "mirror")
	ctx := t.Context()
	const repo = "ubuntu/noble"

	if err := s.SetRef(ctx, repo, "1754400000-aaaa"); err != nil {
		t.Fatalf("SetRef: %v", err)
	}
	ref, err := s.GetRef(ctx, repo, "")
	if err != nil {
		t.Fatalf("GetRef: %v", err)
	}

	for range 3 {
		if _, err := s.GetRef(ctx, repo, ref.ETag); !errors.Is(err, blobstore.ErrNotModified) {
			t.Fatalf("GetRef: got %v, want ErrNotModified", err)
		}
	}

	if err := s.SetRef(ctx, repo, "1754400100-bbbb"); err != nil {
		t.Fatalf("SetRef: %v", err)
	}
	updated, err := s.GetRef(ctx, repo, ref.ETag)
	if err != nil {
		t.Fatalf("GetRef after change: %v", err)
	}
	if updated.Revision != "1754400100-bbbb" {
		t.Fatalf("revision = %q, want 1754400100-bbbb", updated.Revision)
	}
}
