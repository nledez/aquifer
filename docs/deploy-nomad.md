# Deploying on Nomad

A complete job: Consul registration, a readiness-based health check, Traefik
tags for exposure, S3 credentials from Vault, and a rolling update that waits
for each edge to finish prefetching before touching the next.

## The job

```hcl
# aquifer-edge.nomad.hcl

variable "image" {
  type        = string
  default     = "ghcr.io/nledez/aquifer:0.1.0"
  description = "Never latest. A rollback has to be a version, not a hope."
}

variable "datacenters" {
  type    = list(string)
  default = ["dc1"]
}

job "aquifer-edge" {
  datacenters = var.datacenters
  type        = "service"

  # One edge per client-facing node.
  group "edge" {
    count = 3

    network {
      port "http"  { to = 8080 }
      port "admin" { to = 8081 }
    }

    # 5 GiB of cache, 1 GiB of pinned metadata headroom, 3 GiB of reserve for
    # in-flight downloads. See docs/configuration.md.
    ephemeral_disk {
      size    = 10240
      migrate = false
      sticky  = true
    }

    update {
      # One edge at a time. Taking two out at once halves capacity during a
      # deploy, and the edges are the whole serving path.
      max_parallel = 1

      health_check = "checks"

      # Long enough for the pinned patterns to prefetch and for /readyz to
      # have meant something for a while. An edge that reports ready the
      # instant it binds has not filled its cache yet.
      min_healthy_time = "90s"

      # Prefetching 7 MiB of metadata across two dozen publications, on a cold
      # cache, over a slow link.
      healthy_deadline  = "5m"
      progress_deadline = "10m"

      auto_revert = true
    }

    restart {
      attempts = 3
      interval = "10m"
      delay    = "30s"
      mode     = "delay"
    }

    service {
      name         = "aquifer"
      port         = "http"
      provider     = "consul"

      tags = [
        "traefik.enable=true",
        "traefik.http.routers.aquifer.rule=Host(`apt.example.net`)",
        "traefik.http.routers.aquifer.entrypoints=websecure",
        "traefik.http.routers.aquifer.tls.certresolver=letsencrypt",
        # Packages reach 90 MiB and a follower can wait minutes on a download
        # another client started. Traefik must not cut that off.
        "traefik.http.services.aquifer.loadbalancer.responseforwarding.flushinterval=-1",
      ]

      # Readiness, not liveness: this is what decides whether the edge takes
      # traffic. It fails while pinned blobs are still missing and when a
      # suite is past its Valid-Until, which is exactly when an edge should be
      # pulled out.
      check {
        name     = "readyz"
        type     = "http"
        port     = "admin"
        path     = "/readyz"
        interval = "15s"
        timeout  = "3s"

        check_restart {
          limit           = 5
          grace           = "120s"
          ignore_warnings = false
        }
      }
    }

    # A second registration so Prometheus can discover the admin port without
    # it being routable from outside.
    service {
      name     = "aquifer-admin"
      port     = "admin"
      provider = "consul"
      tags     = ["prometheus", "metrics"]

      check {
        name     = "healthz"
        type     = "http"
        path     = "/healthz"
        interval = "30s"
        timeout  = "2s"
      }
    }

    task "aquifer" {
      driver = "docker"

      config {
        image = var.image
        ports = ["http", "admin"]
        args  = ["serve", "--config", "/local/config.yaml"]

        readonly_rootfs = true
        cap_drop        = ["ALL"]
        security_opt    = ["no-new-privileges:true"]

        # The only writable path the edge needs.
        mount {
          type   = "bind"
          source = "local/cache"
          target = "/var/cache/aquifer"
        }
      }

      # Credentials never touch the configuration file or the job spec.
      vault {
        policies    = ["aquifer-edge"]
        change_mode = "restart"
      }

      template {
        destination = "secrets/s3.env"
        env         = true
        change_mode = "restart"
        data        = <<-EOT
          {{ with secret "kv/data/aquifer/s3" }}
          AQUIFER_S3_ACCESS_KEY={{ .Data.data.access_key }}
          AQUIFER_S3_SECRET_KEY={{ .Data.data.secret_key }}
          {{ end }}
        EOT
      }

      template {
        destination = "local/config.yaml"
        change_mode = "restart"
        data        = <<-EOT
          listen: "0.0.0.0:8080"
          # Bound to all interfaces so Consul and Prometheus can reach it. The
          # port is not published outside the cluster network.
          admin_listen: "0.0.0.0:8081"

          log:
            format: json
            level: info

          s3:
            endpoint: https://s3.example.net
            bucket: aquifer
            prefix: mirror
            path_style: true

          poll_interval: 15s
          window: 5
          prefetch_concurrency: 4

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
              - "dists/**"

          repos:
            - repo: debian/bookworm
              prefix: debian/bookworm
            - repo: debian/trixie
              prefix: debian/trixie
            - repo: ubuntu/noble
              prefix: ubuntu/noble
        EOT
      }

      resources {
        cpu    = 500
        memory = 256
      }
    }
  }
}
```

```sh
nomad job validate aquifer-edge.nomad.hcl
nomad job plan     aquifer-edge.nomad.hcl
nomad job run      aquifer-edge.nomad.hcl
```

## Publishing as a periodic job

The master runs the same binary, on a schedule, with no ports and no cache.

```hcl
job "aquifer-publish" {
  datacenters = ["dc1"]
  type        = "batch"

  periodic {
    cron             = "17 * * * *"
    prohibit_overlap = true
  }

  group "publish" {
    task "publish" {
      driver = "docker"

      config {
        image      = "ghcr.io/nledez/aquifer:0.1.0"
        entrypoint = ["/aquifer"]
        args       = ["publish", "--repo", "debian/bookworm", "--json", "/publication"]

        readonly_rootfs = true

        mount {
          type     = "bind"
          source   = "/var/lib/aptly/public/bookworm"
          target   = "/publication"
          readonly = true
        }
      }

      vault { policies = ["aquifer-publish"] }

      template {
        destination = "secrets/s3.env"
        env         = true
        data        = <<-EOT
          {{ with secret "kv/data/aquifer/s3" }}
          AQUIFER_S3_ACCESS_KEY={{ .Data.data.access_key }}
          AQUIFER_S3_SECRET_KEY={{ .Data.data.secret_key }}
          {{ end }}
          AQUIFER_S3_ENDPOINT=https://s3.example.net
          AQUIFER_S3_BUCKET=aquifer
          AQUIFER_S3_PREFIX=mirror
        EOT
      }

      resources {
        cpu    = 1000
        memory = 256
      }
    }
  }
}
```

Garbage collection is the same shape with `args = ["gc", "--keep", "5",
"--json"]` and a daily cron. `--keep` must be at least the edges' `window`.

## Why `min_healthy_time` matters here

`/readyz` reports ready only once every manifest is loaded **and** every pinned
blob is on disk. On a cold cache that means downloading the metadata of every
publication first — a few MiB, but over a slow link and against a possibly
distant object store.

With `min_healthy_time` too short, Nomad marks an edge healthy the moment the
check first passes and immediately proceeds to the next allocation. Set it high
enough that "healthy" means "has been serving correctly for a while", not "has
just bound its port".

`healthy_deadline` bounds the other side: an edge that cannot reach object
storage never becomes ready, and the deploy should fail rather than hang.

## Vault policy

```hcl
# aquifer-edge: read-only, and only what it needs.
path "kv/data/aquifer/s3" {
  capabilities = ["read"]
}
```

The publish job needs the same path, with credentials that may write to the
bucket. Use two separate secrets and two policies if your object store
supports distinct credentials: the edges never need write access.

## Verification

The service registered and its check passes:

```sh
nomad job status aquifer-edge
consul catalog services | grep aquifer
dig +short @127.0.0.1 -p 8600 aquifer.service.consul
```

Every allocation reports the same revision. A divergence here is the signal
described in [operations.md](operations.md#when-an-edge-diverges):

```sh
for alloc in $(nomad job allocs -json aquifer-edge | jq -r '.[].ID'); do
  addr=$(nomad alloc status -json "$alloc" | jq -r '.AllocatedResources.Shared.Ports[] | select(.Label=="admin") | "\(.HostIP):\(.Value)"')
  printf '%s ' "${alloc:0:8}"
  curl -s "http://$addr/metrics" | grep '^aquifer_manifest_revision_info'
done
```

The rolling update really is one at a time:

```sh
nomad job deployments aquifer-edge
nomad deployment status -verbose <deployment-id>
```

Traefik is routing:

```sh
curl -sI https://apt.example.net/debian/bookworm/dists/bookworm/InRelease \
  | grep -iE '^(HTTP|etag)'
```

Then the check that proves it all, from a client:

```sh
sudo tee /etc/apt/sources.list.d/aquifer.sources <<'EOF'
Types: deb
URIs: https://apt.example.net/debian/bookworm
Suites: bookworm
Components: main contrib
Signed-By: /usr/share/keyrings/your-archive-keyring.gpg
EOF

sudo apt update
sudo apt install --reinstall -y hello && hello
```

Finally, confirm a deploy does not interrupt clients. Start a large download,
trigger a rolling update, and watch it complete:

```sh
curl -o /dev/null https://apt.example.net/debian/bookworm/pool/main/l/linux/linux-image-6.1.0-18-amd64_6.1.76-1_amd64.deb &
nomad job run aquifer-edge.nomad.hcl
wait
```
