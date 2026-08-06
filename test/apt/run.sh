#!/usr/bin/env bash
# End-to-end test: a real Debian repository, published to object storage,
# served by an Aquifer edge, consumed by a real apt.
#
# This is the only test that proves the whole thing works. Everything else
# checks a part in isolation; here apt does what apt does, including GPG
# verification, by-hash acquisition and dependency resolution, and the packages
# it installs have to actually run.
#
#   make test-apt
#
# Requires Docker with binfmt support for cross-architecture containers.

set -euo pipefail

SUITES=("${AQUIFER_TEST_SUITES:-bookworm trixie}")
ARCHS=("${AQUIFER_TEST_ARCHS:-amd64 arm64}")
# shellcheck disable=SC2206
SUITES=(${SUITES[*]})
# shellcheck disable=SC2206
ARCHS=(${ARCHS[*]})

IMAGE="${AQUIFER_IMAGE:-aquifer:apt-test}"
MINIO_IMAGE="${AQUIFER_MINIO_IMAGE:-minio/minio:RELEASE.2025-04-22T22-12-26Z}"
NETWORK="aquifer-apt-test"
WORK="$(mktemp -d)"
REPO="$WORK/repo"

# The package to install. It is tiny, it prints something, and its only
# dependency is a libc the base image already has, so a repository containing
# nothing else is enough to install it.
PACKAGE="${AQUIFER_TEST_PACKAGE:-hello}"

log()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() { printf '\033[31mFAIL: %s\033[0m\n' "$*" >&2; exit 1; }

cleanup() {
  # The EXIT trap's own status becomes the script's, so a cleanup that trips
  # reports a failure the test never had. Carry the real result through, and
  # say out loud what was left behind rather than swallowing it.
  local status=$?
  docker rm -f aquifer-apt-edge aquifer-apt-minio >/dev/null 2>&1 || true
  docker volume rm aquifer-apt-cache >/dev/null 2>&1 || true
  docker network rm "$NETWORK" >/dev/null 2>&1 || true
  rm -rf "$WORK" || printf 'warning: %s could not be removed\n' "$WORK" >&2
  return "$status"
}
trap cleanup EXIT

# --- 1. build a real Debian repository ----------------------------------------

log "Building a signed Debian repository for: ${SUITES[*]} / ${ARCHS[*]}"

for suite in "${SUITES[@]}"; do
  mkdir -p "$REPO/$suite"
  # The repository for a suite is built inside that suite's own container, so
  # the packages and the tooling match what a real publisher would use.
  docker run --rm \
    -v "$REPO/$suite:/repo" \
    -e SUITE="$suite" \
    -e ARCHS="${ARCHS[*]}" \
    -e PACKAGE="$PACKAGE" \
    -e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" \
    "debian:$suite" \
    bash -euo pipefail -c '
      export DEBIAN_FRONTEND=noninteractive
      apt-get update -qq
      apt-get install -y -qq --no-install-recommends \
        dpkg-dev apt-utils gnupg ca-certificates >/dev/null

      for arch in $ARCHS; do
        dpkg --add-architecture "$arch"
      done
      apt-get update -qq

      mkdir -p /repo/pool/main
      cd /tmp
      for arch in $ARCHS; do
        apt-get download "${PACKAGE}:${arch}" >/dev/null
      done
      mv ./*.deb /repo/pool/main/

      cd /repo
      for arch in $ARCHS; do
        mkdir -p "dists/$SUITE/main/binary-$arch"
        dpkg-scanpackages --arch "$arch" pool/main 2>/dev/null \
          > "dists/$SUITE/main/binary-$arch/Packages"
        gzip -9nkf "dists/$SUITE/main/binary-$arch/Packages"

        # by-hash copies, the way a real publisher keeps them. Aquifer does not
        # publish these paths: it resolves them from the digest in the URL, so
        # apt asking for them exercises exactly that.
        for variant in Packages Packages.gz; do
          f="dists/$SUITE/main/binary-$arch/$variant"
          mkdir -p "dists/$SUITE/main/binary-$arch/by-hash/SHA256"
          cp "$f" "dists/$SUITE/main/binary-$arch/by-hash/SHA256/$(sha256sum "$f" | cut -d" " -f1)"
        done
      done

      apt-ftparchive \
        -o APT::FTPArchive::Release::Origin=AquiferTest \
        -o APT::FTPArchive::Release::Label=AquiferTest \
        -o APT::FTPArchive::Release::Suite="$SUITE" \
        -o APT::FTPArchive::Release::Codename="$SUITE" \
        -o APT::FTPArchive::Release::Architectures="$ARCHS" \
        -o APT::FTPArchive::Release::Components=main \
        -o APT::FTPArchive::Release::Acquire-By-Hash=yes \
        release "dists/$SUITE" > "dists/$SUITE/Release"

      # A throwaway signing key, generated per run.
      export GNUPGHOME=/tmp/gnupg
      mkdir -p "$GNUPGHOME" && chmod 700 "$GNUPGHOME"
      gpg --batch --quiet --passphrase "" \
        --quick-gen-key "Aquifer Test <test@example.invalid>" default default never
      gpg --batch --quiet --yes --clearsign -o "dists/$SUITE/InRelease" "dists/$SUITE/Release"
      gpg --batch --quiet --yes --detach-sign --armor -o "dists/$SUITE/Release.gpg" "dists/$SUITE/Release"

      # The key is served as an ordinary file: no index mentions it, so Aquifer
      # has to hash it itself.
      mkdir -p /repo/keys
      gpg --batch --quiet --armor --export > "/repo/keys/$SUITE.asc"

      # Everything above ran as root, which on a Linux host means the bind
      # mount now holds root-owned files the caller cannot delete: removing a
      # file needs write permission on its directory, and those are root-owned
      # too. Hand the tree back before leaving, or cleanup fails on a run that
      # otherwise passed.
      chown -R "$HOST_UID:$HOST_GID" /repo
    ' >/dev/null

  [ -s "$REPO/$suite/dists/$suite/InRelease" ] || fail "no InRelease was produced for $suite"
  echo "  $suite: $(find "$REPO/$suite/pool" -name '*.deb' | wc -l | tr -d ' ') package(s), signed"
done

# --- 2. object storage ---------------------------------------------------------

log "Starting MinIO"
docker network create "$NETWORK" >/dev/null
docker run -d --rm --name aquifer-apt-minio --network "$NETWORK" \
  -e MINIO_ROOT_USER=aquifer -e MINIO_ROOT_PASSWORD=aquifer-secret \
  "$MINIO_IMAGE" server /data >/dev/null

for _ in $(seq 1 60); do
  if docker exec aquifer-apt-minio mc ready local >/dev/null 2>&1; then break; fi
  sleep 1
done
docker exec aquifer-apt-minio mc alias set local http://127.0.0.1:9000 aquifer aquifer-secret >/dev/null 2>&1
docker exec aquifer-apt-minio mc mb --ignore-existing local/aquifer >/dev/null

# --- 3. publish ----------------------------------------------------------------

log "Publishing each suite"
for suite in "${SUITES[@]}"; do
  docker run --rm --network "$NETWORK" \
    -v "$REPO/$suite:/publication:ro" \
    -e AQUIFER_S3_ENDPOINT=http://aquifer-apt-minio:9000 \
    -e AQUIFER_S3_BUCKET=aquifer \
    -e AQUIFER_S3_PREFIX=mirror \
    -e AQUIFER_S3_INSECURE=1 \
    -e AQUIFER_S3_ACCESS_KEY=aquifer \
    -e AQUIFER_S3_SECRET_KEY=aquifer-secret \
    --entrypoint /aquifer "$IMAGE" \
    publish --repo "debian/$suite" /publication
done

# --- 4. the edge ---------------------------------------------------------------

log "Starting the edge"
{
  echo 'listen: "0.0.0.0:8080"'
  echo 'admin_listen: "0.0.0.0:8081"'
  echo 'log: {format: text, level: info}'
  echo 's3: {endpoint: "http://aquifer-apt-minio:9000", bucket: aquifer, prefix: mirror, path_style: true, insecure: true}'
  echo 'poll_interval: 5s'
  echo 'window: 5'
  echo 'cache:'
  echo '  dir: /var/cache/aquifer'
  echo '  max_size: 256MiB'
  echo '  pinned_max_size: 32MiB'
  echo '  temp_reserve: 128MiB'
  echo '  pinned: ["**/dists/**"]'
  echo '  prefetch: ["**/dists/**"]'
  echo 'repos:'
  for suite in "${SUITES[@]}"; do
    echo "  - {repo: debian/$suite, prefix: debian/$suite}"
  done
} > "$WORK/config.yaml"

docker volume create aquifer-apt-cache >/dev/null
docker run -d --name aquifer-apt-edge --network "$NETWORK" \
  --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  -v aquifer-apt-cache:/var/cache/aquifer \
  -v "$WORK/config.yaml":/etc/aquifer/config.yaml:ro \
  -e AQUIFER_S3_ACCESS_KEY=aquifer -e AQUIFER_S3_SECRET_KEY=aquifer-secret \
  "$IMAGE" serve --config /etc/aquifer/config.yaml >/dev/null

for _ in $(seq 1 60); do
  status=$(docker inspect --format '{{.State.Health.Status}}' aquifer-apt-edge 2>/dev/null || echo starting)
  [ "$status" = "healthy" ] && break
  sleep 1
done
[ "$(docker inspect --format '{{.State.Health.Status}}' aquifer-apt-edge)" = "healthy" ] \
  || { docker logs aquifer-apt-edge; fail "the edge never became healthy"; }
echo "  edge is healthy"

# --- 5. apt ---------------------------------------------------------------------


# The client bootstrap is shared by the install runs and the by-hash check.
cat > "$WORK/client-setup.sh" <<'SETUP'
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

# Only Aquifer. Nothing may be satisfied by an upstream mirror.
rm -f /etc/apt/sources.list /etc/apt/sources.list.d/*
mkdir -p /etc/apt/keyrings

cat > /etc/apt/sources.list.d/aquifer.sources <<EOF
Types: deb
URIs: http://aquifer-apt-edge:8080/debian/$SUITE
Suites: $SUITE
Components: main
Signed-By: /etc/apt/keyrings/aquifer.asc
EOF

# The signing key comes off the edge itself. No index mentions it, so Aquifer
# had to hash it during publish to be able to serve it at all.
exec 3<>/dev/tcp/aquifer-apt-edge/8080
printf "GET /debian/%s/keys/%s.asc HTTP/1.0\r\nHost: aquifer-apt-edge\r\n\r\n" "$SUITE" "$SUITE" >&3
sed -e "1,/^\r*$/d" <&3 > /etc/apt/keyrings/aquifer.asc
exec 3<&-
grep -q "BEGIN PGP PUBLIC KEY BLOCK" /etc/apt/keyrings/aquifer.asc
SETUP

failures=0
for suite in "${SUITES[@]}"; do
  for arch in "${ARCHS[@]}"; do
    log "apt update && apt install $PACKAGE  ($suite / $arch)"

    if docker run --rm --platform "linux/$arch" --network "$NETWORK" \
      -e SUITE="$suite" -e PACKAGE="$PACKAGE" \
      -v "$WORK/client-setup.sh:/setup.sh:ro" \
      "debian:$suite" \
      bash -euo pipefail -c '
        . /setup.sh
        apt-get update
        apt-get install -y --no-install-recommends "$PACKAGE"

        # The package has to actually work, not merely unpack.
        command -v "$PACKAGE" >/dev/null
        "$PACKAGE" | head -1
        dpkg-query -W -f "installed: \${Package} \${Version} \${Architecture}\n" "$PACKAGE"
      '
    then
      echo "  OK: $suite / $arch"
    else
      echo "  FAILED: $suite / $arch"
      failures=$((failures + 1))
    fi
  done
done


# --- 5b. by-hash ------------------------------------------------------------------

# SPEC section 5 asks for by-hash resolution under SHA256. Real apt asks for
# the strongest digest the Release declares, which for both apt-ftparchive and
# aptly is SHA512, and falls back to the plain path when that 404s. Both facts
# are asserted here: the SHA256 form resolves, and apt's preference does not
# break acquisition.
log "Checking by-hash resolution"

SUITE0="${SUITES[0]}"
ARCH0="${ARCHS[0]}"
PKG_SHA256=$(sha256sum "$REPO/$SUITE0/dists/$SUITE0/main/binary-$ARCH0/Packages" | cut -d' ' -f1)

docker run --rm --network "$NETWORK" \
  -e SUITE="$SUITE0" -e ARCH="$ARCH0" -e PKG_SHA256="$PKG_SHA256" \
  -v "$WORK/client-setup.sh:/setup.sh:ro" \
  "debian:$SUITE0" \
  bash -euo pipefail -c '
    . /setup.sh

    fetch() {
      exec 3<>/dev/tcp/aquifer-apt-edge/8080
      printf "GET %s HTTP/1.0\r\nHost: aquifer-apt-edge\r\n\r\n" "$1" >&3
      cat <&3
      exec 3<&-
    }

    # The SHA256 form must resolve straight from the digest in the URL.
    base="/debian/$SUITE/dists/$SUITE/main/binary-$ARCH/by-hash/SHA256"
    status=$(fetch "$base/$PKG_SHA256" | head -1)
    echo "  by-hash SHA256: $status"
    case "$status" in *" 200 "*) ;; *) echo "by-hash SHA256 did not resolve"; exit 1;; esac

    body_sha=$(fetch "$base/$PKG_SHA256" | sed -e "1,/^\r*$/d" | sha256sum | cut -d" " -f1)
    [ "$body_sha" = "$PKG_SHA256" ] || { echo "by-hash served the wrong bytes"; exit 1; }
    echo "  by-hash SHA256 body matches its digest"

    # A digest no revision references must not become a way to make the edge
    # pull arbitrary objects out of storage.
    unknown=$(printf "not published" | sha256sum | cut -d" " -f1)
    status=$(fetch "$base/$unknown" | head -1)
    echo "  by-hash for an unpublished digest: $status"
    case "$status" in *" 404 "*) ;; *) echo "an unknown digest was not refused"; exit 1;; esac

    # What apt actually asks for, and that acquisition succeeds regardless.
    apt-get update -o Debug::Acquire::http=1 > /tmp/acquire.log 2>&1
    algos=$(grep -oE "by-hash/[A-Z0-9]+" /tmp/acquire.log | sort -u | tr "\n" " ")
    echo "  apt requested by-hash under: ${algos:-none}"
    apt-get install -y --no-install-recommends hello >/dev/null
    echo "  acquisition succeeded"
  ' || fail "by-hash resolution is broken"

# --- 6. what the edge saw --------------------------------------------------------

log "Edge metrics"
docker run --rm --network "$NETWORK" "debian:${SUITES[0]}" \
  bash -c 'exec 3<>/dev/tcp/aquifer-apt-edge/8081
    printf "GET /metrics HTTP/1.0\r\nHost: aquifer-apt-edge\r\n\r\n" >&3
    sed -e "1,/^\r*$/d" <&3' \
  | grep -E '^aquifer_(cache_requests_total|fetch_coalesced_readers_total|cache_pinned_objects|manifest_revision_info)' \
  | grep -v ' 0$' || true

if [ "$failures" -ne 0 ]; then
  docker logs aquifer-apt-edge
  fail "$failures suite/architecture combination(s) failed"
fi

log "All ${#SUITES[@]} suite(s) x ${#ARCHS[@]} architecture(s) installed successfully"
