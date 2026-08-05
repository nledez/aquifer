package blobstore

import "testing"

const testHash = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func TestBlobKeyShardsByTheFirstTwoBytes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		prefix string
		want   string
	}{
		{"", "blobs/sha256/ab/cd/" + testHash},
		{"mirror", "mirror/blobs/sha256/ab/cd/" + testHash},
		{"mirror/", "mirror/blobs/sha256/ab/cd/" + testHash},
		{"/mirror/nested/", "mirror/nested/blobs/sha256/ab/cd/" + testHash},
	}
	for _, tc := range cases {
		if got := BlobKey(tc.prefix, testHash); got != tc.want {
			t.Fatalf("BlobKey(%q) = %q, want %q", tc.prefix, got, tc.want)
		}
	}
}

func TestManifestAndRefKeys(t *testing.T) {
	t.Parallel()

	if got, want := ManifestKey("mirror", "debian/bookworm", "1754400000-aaaa"),
		"mirror/manifests/debian/bookworm/1754400000-aaaa.tsv.zst"; got != want {
		t.Fatalf("ManifestKey = %q, want %q", got, want)
	}
	if got, want := RefKey("mirror", "debian/bookworm"),
		"mirror/refs/debian/bookworm/current"; got != want {
		t.Fatalf("RefKey = %q, want %q", got, want)
	}
	if got, want := RefKey("", "salt"), "refs/salt/current"; got != want {
		t.Fatalf("RefKey = %q, want %q", got, want)
	}
}

func TestHashFromBlobKeyRoundTrips(t *testing.T) {
	t.Parallel()

	got, ok := HashFromBlobKey("mirror", BlobKey("mirror", testHash))
	if !ok || got != testHash {
		t.Fatalf("HashFromBlobKey = %q, %v; want %q, true", got, ok, testHash)
	}
}

func TestHashFromBlobKeyRejectsMalformedKeys(t *testing.T) {
	t.Parallel()

	bad := []string{
		"mirror/blobs/sha256/ab/cd/short",
		"mirror/blobs/sha256/" + testHash,                  // unsharded
		"mirror/blobs/sha512/ab/cd/" + testHash,            // wrong algorithm
		"mirror/blobs/sha256/zz/cd/" + testHash,            // shard is not hex
		"mirror/blobs/sha256/ff/cd/" + testHash,            // shard disagrees with the hash
		"other/blobs/sha256/ab/cd/" + testHash,             // wrong prefix
		"mirror/manifests/debian/1754400000-aaaa.tsv.zst",  // not a blob at all
	}
	for _, key := range bad {
		if got, ok := HashFromBlobKey("mirror", key); ok {
			t.Fatalf("HashFromBlobKey(%q) = %q, true; want false", key, got)
		}
	}
}

func TestRepoFromRefKeyRoundTrips(t *testing.T) {
	t.Parallel()

	for _, repo := range []string{"salt", "debian/bookworm", "ubuntu/noble"} {
		got, ok := RepoFromRefKey("mirror", RefKey("mirror", repo))
		if !ok || got != repo {
			t.Fatalf("RepoFromRefKey(%q) = %q, %v; want %q, true", repo, got, ok, repo)
		}
	}
	if _, ok := RepoFromRefKey("mirror", "mirror/refs/debian/bookworm/other"); ok {
		t.Fatal("RepoFromRefKey accepted a key that is not a current ref")
	}
}

func TestRevisionFromManifestKeyRoundTrips(t *testing.T) {
	t.Parallel()

	key := ManifestKey("mirror", "debian/bookworm", "1754400000-aaaa")
	got, ok := RevisionFromManifestKey("mirror", "debian/bookworm", key)
	if !ok || got != "1754400000-aaaa" {
		t.Fatalf("RevisionFromManifestKey = %q, %v; want the revision", got, ok)
	}
	if _, ok := RevisionFromManifestKey("mirror", "debian/bookworm", key+".bak"); ok {
		t.Fatal("RevisionFromManifestKey accepted a key without the expected suffix")
	}
}
