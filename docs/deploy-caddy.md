# Deploying behind Caddy

Caddy obtains and renews the certificate on its own, which makes it the
shortest path to a working TLS front end.

## The complete Caddyfile

```caddyfile
# /etc/caddy/Caddyfile

{
	# Where Let's Encrypt sends expiry warnings.
	email ops@example.net
}

apt.example.net {
	encode zstd gzip

	log {
		output file /var/log/caddy/aquifer.log
		format json
	}

	# The admin port is not for clients.
	handle /metrics* {
		respond 404
	}
	handle /healthz* {
		respond 404
	}
	handle /readyz* {
		respond 404
	}

	handle {
		reverse_proxy 127.0.0.1:8080 {
			# Send each chunk on as it arrives instead of accumulating a whole
			# 90 MiB blob first. This is the Caddy equivalent of nginx's
			# proxy_buffering off, and it is the single most important line
			# here.
			flush_interval -1

			# A follower waiting on a download another client started
			# legitimately holds the connection open for minutes.
			transport http {
				dial_timeout 10s
				response_header_timeout 60s
				read_timeout 600s
				write_timeout 600s
				keepalive 60s
				keepalive_idle_conns 32
			}

			header_up X-Real-IP {remote_host}
			header_up X-Forwarded-For {remote_host}
			header_up X-Forwarded-Proto {scheme}
			header_up Host {host}
		}
	}
}
```

```sh
sudo caddy validate --config /etc/caddy/Caddyfile
sudo systemctl reload caddy
```

## Do not add a cache

Caddy has no response cache built in, which is convenient: the mistake takes
deliberate effort. Do not make it — no `cache-handler`, no
`souin`, nothing in front of the edge.

The reasons are the same as for nginx, and they are not about performance:

1. **A second copy on the same disk, outside the accounting.** `max_size` is
   what you size the volume against and tune from `aquifer_cache_bytes`. A
   proxy cache doubles the footprint and reports none of it.

2. **It reintroduces invalidation.** Aquifer is content-addressed: a path
   changes content only when a new revision makes it a different blob, and the
   ETag changes with it. A URL-keyed, timer-expired cache will serve last
   revision's `InRelease` beside this revision's `Packages` during a switch,
   and apt rejects that with a hash mismatch — from a mirror that is in fact
   perfectly consistent.

3. **It blinds the metrics.** Requests answered by the proxy never reach the
   edge, so `aquifer_cache_requests_total` stops describing what clients asked
   for, and the hit ratio you tune from becomes fiction.

`encode zstd gzip` above is fine and unrelated: it compresses the transfer, it
does not store anything. In practice it does almost nothing here, since
packages are already compressed.

## Running Caddy and the edge together

If both run under systemd on the same host, the edge should bind loopback only:

```yaml
listen: "127.0.0.1:8080"
admin_listen: "127.0.0.1:8081"
```

With Caddy in a container and the edge alongside, put them on one network and
point `reverse_proxy` at the service name:

```caddyfile
reverse_proxy edge:8080 {
	flush_interval -1
	...
}
```

## Verification

TLS is live and the proxy is forwarding:

```sh
curl -sI https://apt.example.net/debian/bookworm/dists/bookworm/InRelease \
  | grep -iE '^(HTTP|etag)'
```

```
HTTP/2 200
etag: "250b42d764b7c442462617af34bc3f1647fb0905006ece639f5d55bc27b1533e"
```

The admin endpoints are not exposed:

```sh
curl -s -o /dev/null -w '%{http_code}\n' https://apt.example.net/metrics
```

```
404
```

Ranges work through the proxy:

```sh
curl -s -D- -r 100-199 -o /dev/null \
  https://apt.example.net/debian/bookworm/pool/main/h/hello/hello_2.10-3_amd64.deb \
  | grep -iE '^(HTTP|content-range)'
```

```
HTTP/2 206
content-range: bytes 100-199/53100
```

Responses stream rather than spool. Watch the bytes arrive over time rather
than all at once at the end:

```sh
curl -s -o /dev/null -w 'ttfb=%{time_starttransfer}s total=%{time_total}s\n' \
  https://apt.example.net/debian/bookworm/pool/main/l/linux/linux-image-6.1.0-18-amd64_6.1.76-1_amd64.deb
```

A time-to-first-byte close to the total time means `flush_interval -1` is
missing.

Then the real check:

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
