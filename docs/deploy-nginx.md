# Deploying behind nginx

nginx terminates TLS and forwards to the edge. The edge does no TLS of its own.

## The complete server block

```nginx
# /etc/nginx/sites-available/aquifer
upstream aquifer_edge {
    server 127.0.0.1:8080;
    keepalive 32;
}

server {
    listen 80;
    listen [::]:80;
    server_name apt.example.net;

    location /.well-known/acme-challenge/ {
        root /var/www/html;
    }
    location / {
        return 301 https://$host$request_uri;
    }
}

server {
    listen 443 ssl;
    listen [::]:443 ssl;
    http2 on;
    server_name apt.example.net;

    ssl_certificate     /etc/letsencrypt/live/apt.example.net/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/apt.example.net/privkey.pem;
    ssl_protocols       TLSv1.2 TLSv1.3;
    ssl_session_cache   shared:SSL:10m;

    access_log /var/log/nginx/aquifer-access.log;
    error_log  /var/log/nginx/aquifer-error.log;

    # Packages reach 90 MiB. There is no request body worth accepting, and no
    # response worth truncating.
    client_max_body_size 1m;

    location / {
        proxy_pass http://aquifer_edge;
        proxy_http_version 1.1;
        proxy_set_header Connection "";

        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # Stream the response through instead of spooling it. Without this,
        # nginx buffers whole 90 MiB blobs to disk before sending a byte:
        # latency for the client, and pointless write amplification on the
        # proxy.
        proxy_buffering off;
        proxy_request_buffering off;

        # A follower waiting on a download another client started legitimately
        # holds the connection open for minutes. This timeout is between bytes,
        # so it does not cap the transfer, only a stall.
        proxy_read_timeout    600s;
        proxy_send_timeout    600s;
        proxy_connect_timeout 10s;

        # apt uses ranges to resume interrupted downloads. The edge answers
        # them; nginx must not strip the header on its way in.
        proxy_set_header Range $http_range;
        proxy_set_header If-Range $http_if_range;
    }

    # The admin port is not for clients. Serve it here only if you have a
    # reason to, and restrict it.
    location /metrics {
        deny all;
    }
}
```

Enable it:

```sh
sudo ln -s /etc/nginx/sites-available/aquifer /etc/nginx/sites-enabled/
sudo nginx -t && sudo systemctl reload nginx
```

## Do not enable `proxy_cache`

This is the one setting that looks obviously right and is not.

```nginx
# Do not do this.
proxy_cache aquifer;
proxy_cache_valid 200 1h;
```

Three reasons:

1. **The edge already caches, with an accounting you rely on.** `max_size` is
   the number you size the disk against and tune from
   `aquifer_cache_bytes` and `aquifer_cache_evictions_total`. An nginx cache
   holds a second, invisible copy of the same bytes on the same disk, so the
   volume needs twice the budget and no metric tells you that.

2. **It reintroduces the invalidation problem Aquifer exists to avoid.**
   Everything Aquifer serves is content-addressed: a path's content changes
   only when a new revision makes it a different blob, and the ETag changes
   with it. `proxy_cache` keys on the URL and expires on a timer, so during a
   revision switch it will serve the previous `InRelease` alongside the new
   `Packages` — a combination apt rejects with a hash mismatch, from a mirror
   that is actually consistent.

3. **It hides the metrics.** A request nginx answers never reaches the edge, so
   `aquifer_cache_requests_total` stops describing what clients asked for. The
   hit ratio you tune the cache budget from becomes a fiction.

If you want nginx to cache *something*, cache nothing. The edge is on the same
host, over loopback.

## Verification

nginx is forwarding, and streaming rather than buffering:

```sh
curl -sI https://apt.example.net/debian/bookworm/dists/bookworm/InRelease \
  | grep -iE '^(HTTP|etag|content-length)'
```

```
HTTP/2 200
etag: "250b42d764b7c442462617af34bc3f1647fb0905006ece639f5d55bc27b1533e"
content-length: 388
```

Ranges survive the proxy:

```sh
curl -s -D- -r 100-199 -o /dev/null \
  https://apt.example.net/debian/bookworm/pool/main/h/hello/hello_2.10-3_amd64.deb \
  | grep -iE '^(HTTP|content-range)'
```

```
HTTP/2 206
content-range: bytes 100-199/53100
```

Revalidation costs nothing:

```sh
etag=$(curl -sI https://apt.example.net/debian/bookworm/dists/bookworm/InRelease \
  | awk -F'"' '/[Ee]tag/{print $2}')
curl -s -o /dev/null -w '%{http_code}\n' -H "If-None-Match: \"$etag\"" \
  https://apt.example.net/debian/bookworm/dists/bookworm/InRelease
```

```
304
```

The client's address reaches the edge rather than nginx's:

```sh
sudo journalctl -u aquifer -n 20 --output cat | grep -i forwarded
```

And finally, the only check that matters:

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

A large package, to exercise the streaming path end to end:

```sh
time apt-get download linux-image-amd64
```

If that stalls for the whole transfer and then completes in a burst,
`proxy_buffering` is still on.
