package blobstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config points at an S3-compatible endpoint. The target may well be Swift
// behind its S3 compatibility layer, so the endpoint, the addressing style and
// the region all have to be settable rather than inferred.
type Config struct {
	// Endpoint is a URL such as https://s3.example.net, or a bare host:port,
	// in which case TLS is assumed unless Insecure is set.
	Endpoint  string
	Bucket    string
	Prefix    string
	AccessKey string
	SecretKey string
	Region    string
	// PathStyle forces bucket-in-path addressing, which most non-AWS
	// implementations need.
	PathStyle bool
	// Insecure allows plain HTTP for a bare host:port endpoint.
	Insecure bool
}

// S3 is a Store backed by an S3-compatible object store.
type S3 struct {
	client *minio.Client
	bucket string
	prefix string
}

// NewS3 connects to the configured endpoint. It does not verify that the
// bucket exists; the first real operation will say so.
func NewS3(cfg Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("blobstore: bucket is required")
	}
	host, secure, err := parseEndpoint(cfg.Endpoint, cfg.Insecure)
	if err != nil {
		return nil, err
	}

	lookup := minio.BucketLookupAuto
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       secure,
		Region:       cfg.Region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("blobstore: connect to %s: %w", cfg.Endpoint, err)
	}
	return &S3{client: client, bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

func parseEndpoint(endpoint string, insecure bool) (host string, secure bool, err error) {
	if endpoint == "" {
		return "", false, errors.New("blobstore: endpoint is required")
	}
	if !strings.Contains(endpoint, "://") {
		return endpoint, !insecure, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", false, fmt.Errorf("blobstore: endpoint %q: %w", endpoint, err)
	}
	switch u.Scheme {
	case "http":
		return u.Host, false, nil
	case "https":
		return u.Host, true, nil
	default:
		return "", false, fmt.Errorf("blobstore: endpoint %q: unsupported scheme %q", endpoint, u.Scheme)
	}
}

func (s *S3) ListBlobs(ctx context.Context) ([]BlobInfo, error) {
	var out []BlobInfo
	prefix := BlobsPrefix(s.prefix)
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, mapErr(obj.Err, "list blobs")
		}
		hash, ok := HashFromBlobKey(s.prefix, obj.Key)
		if !ok {
			// Not something we wrote. Skipping keeps the GC from ever
			// deleting an object it does not understand.
			continue
		}
		out = append(out, BlobInfo{Hash: hash, Size: obj.Size, LastModified: obj.LastModified})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b BlobInfo) int { return strings.Compare(a.Hash, b.Hash) })
	return out, nil
}

func (s *S3) StatBlob(ctx context.Context, hash string) (BlobInfo, error) {
	info, err := s.client.StatObject(ctx, s.bucket, BlobKey(s.prefix, hash), minio.StatObjectOptions{})
	if err != nil {
		return BlobInfo{}, mapErr(err, "blob "+hash)
	}
	return BlobInfo{Hash: hash, Size: info.Size, LastModified: info.LastModified}, nil
}

func (s *S3) PutBlob(ctx context.Context, hash string, r io.Reader, size int64) error {
	_, err := s.client.PutObject(ctx, s.bucket, BlobKey(s.prefix, hash), r, size,
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	return mapErr(err, "put blob "+hash)
}

func (s *S3) GetBlob(ctx context.Context, hash string) (io.ReadCloser, error) {
	return s.get(ctx, BlobKey(s.prefix, hash), "blob "+hash, minio.GetObjectOptions{})
}

func (s *S3) DeleteBlob(ctx context.Context, hash string) error {
	err := s.client.RemoveObject(ctx, s.bucket, BlobKey(s.prefix, hash), minio.RemoveObjectOptions{})
	if isNotFound(err) {
		return nil
	}
	return mapErr(err, "delete blob "+hash)
}

func (s *S3) PutManifest(ctx context.Context, repo, revision string, r io.Reader, size int64) error {
	_, err := s.client.PutObject(ctx, s.bucket, ManifestKey(s.prefix, repo, revision), r, size,
		minio.PutObjectOptions{ContentType: "application/zstd"})
	return mapErr(err, "put manifest "+repo+"/"+revision)
}

func (s *S3) GetManifest(ctx context.Context, repo, revision string) (io.ReadCloser, error) {
	return s.get(ctx, ManifestKey(s.prefix, repo, revision),
		"manifest "+repo+"/"+revision, minio.GetObjectOptions{})
}

func (s *S3) ListManifests(ctx context.Context, repo string) ([]ManifestInfo, error) {
	var out []ManifestInfo
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    ManifestsPrefix(s.prefix, repo),
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, mapErr(obj.Err, "list manifests of "+repo)
		}
		revision, ok := RevisionFromManifestKey(s.prefix, repo, obj.Key)
		if !ok {
			continue
		}
		out = append(out, ManifestInfo{
			Revision:     revision,
			Size:         obj.Size,
			LastModified: obj.LastModified,
		})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b ManifestInfo) int { return strings.Compare(a.Revision, b.Revision) })
	return out, nil
}

func (s *S3) DeleteManifest(ctx context.Context, repo, revision string) error {
	err := s.client.RemoveObject(ctx, s.bucket, ManifestKey(s.prefix, repo, revision),
		minio.RemoveObjectOptions{})
	if isNotFound(err) {
		return nil
	}
	return mapErr(err, "delete manifest "+repo+"/"+revision)
}

func (s *S3) SetRef(ctx context.Context, repo, revision string) error {
	body := revision + "\n"
	_, err := s.client.PutObject(ctx, s.bucket, RefKey(s.prefix, repo),
		strings.NewReader(body), int64(len(body)),
		minio.PutObjectOptions{ContentType: "text/plain"})
	return mapErr(err, "set ref "+repo)
}

func (s *S3) GetRef(ctx context.Context, repo, ifNoneMatch string) (Ref, error) {
	opts := minio.GetObjectOptions{}
	if ifNoneMatch != "" {
		if err := opts.SetMatchETagExcept(ifNoneMatch); err != nil {
			return Ref{}, fmt.Errorf("blobstore: ref %s: %w", repo, err)
		}
	}

	obj, err := s.client.GetObject(ctx, s.bucket, RefKey(s.prefix, repo), opts)
	if err != nil {
		return Ref{}, mapErr(err, "ref "+repo)
	}
	defer func() { _ = obj.Close() }()

	info, err := obj.Stat()
	if err != nil {
		return Ref{}, mapErr(err, "ref "+repo)
	}
	body, err := io.ReadAll(obj)
	if err != nil {
		return Ref{}, mapErr(err, "read ref "+repo)
	}

	revision := strings.TrimSpace(string(body))
	if revision == "" {
		return Ref{}, fmt.Errorf("blobstore: ref %s is empty", repo)
	}
	return Ref{Revision: revision, ETag: info.ETag}, nil
}

func (s *S3) ListRepos(ctx context.Context) ([]string, error) {
	var out []string
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    RefsPrefix(s.prefix),
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, mapErr(obj.Err, "list repos")
		}
		repo, ok := RepoFromRefKey(s.prefix, obj.Key)
		if !ok {
			continue
		}
		out = append(out, repo)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	slices.Sort(out)
	return out, nil
}

// get opens an object, forcing the request now so that a missing object is
// reported here rather than on the caller's first Read.
func (s *S3) get(ctx context.Context, key, what string, opts minio.GetObjectOptions) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, opts)
	if err != nil {
		return nil, mapErr(err, what)
	}
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		return nil, mapErr(err, what)
	}
	return obj, nil
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	resp := minio.ToErrorResponse(err)
	return resp.StatusCode == http.StatusNotFound ||
		resp.Code == "NoSuchKey" || resp.Code == "NoSuchBucket"
}

func mapErr(err error, what string) error {
	if err == nil {
		return nil
	}
	resp := minio.ToErrorResponse(err)
	if resp.StatusCode == http.StatusNotModified {
		return ErrNotModified
	}
	if isNotFound(err) {
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	}
	return fmt.Errorf("blobstore: %s: %w", what, err)
}
