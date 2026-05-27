# nextcloud-appstore-relay

A small Docker-friendly relay that lets an air-gapped Nextcloud install and
update apps from the official Nextcloud App Store
(`https://apps.nextcloud.com/api/v1`) without giving the Nextcloud host
itself any outbound internet access. The relay sits in a DMZ, talks to the
upstream store and to GitHub/etc. on Nextcloud's behalf, and exposes a
byte-identical view of the catalog and tarballs to Nextcloud.

The relay does two things, and only two things:

1. **Rewrites the `download` URL of every app release** in `apps.json` to
   point back at itself, so Nextcloud — which would otherwise try to reach
   `github.com` directly — fetches the tarball from the relay.
2. **Streams the original tarball bytes through unchanged.** No
   decompression, no re-archiving, no transformation. Nextcloud's
   signature check (SHA-512 / `openssl_verify` against the app's
   certificate in the catalog) is therefore unaffected — see
   [`LIMITATIONS.md`](LIMITATIONS.md) for the upstream code reference.

Everything else — caching, logging, conditional GETs — is implementation
detail.

---

## Quick start (Ubuntu / Debian)

Two commands, on the host that will run the relay:

```bash
git clone https://github.com/AndreWaidelich/nextcloud-appstore-relay.git
cd nextcloud-appstore-relay
sudo make install
```

The installer (`scripts/install.sh`) is idempotent and does this:

1. Detects Ubuntu/Debian.
2. Installs missing deps: `curl`, `jq`, `docker.io`, `docker-compose-v2`.
3. Asks for `RELAY_PUBLIC_URL` (the URL your Nextcloud will hit). Default
   is `http://<host-ip>:8080` for a plain LAN test.
4. Writes `.env`.
5. Runs the upstream API sanity check (`make api-check`).
6. `docker compose up -d --build`.
7. Waits for `/healthz` to come up and prints the next-step `occ`
   command for the Nextcloud side.

When it's done, point your Nextcloud at the relay — once, on the Nextcloud
host:

```bash
sudo -u www-data php occ config:system:set appstoreurl \
    --value="http://<relay-ip>:8080"
```

Open Nextcloud → Admin → Apps, install or update an app. The relay log
(`make logs`) shows `tarball cache miss -> cached` on first install,
`tarball cache hit` for repeats.

### Non-interactive install

```bash
sudo make install-yes              # uses host's primary IP on :8080
# or:
sudo bash scripts/install.sh --public-url http://10.0.0.5:8080 --yes
```

---

## Day-to-day commands

```
make logs        # follow container logs
make status      # container status + cache size on disk
make api-check   # canary: does upstream API still match the relay's assumptions?
make restart     # apply changes from .env
make update      # git pull + rebuild + restart
make clean-cache # nuke the disk cache (relay re-fetches on next request)
make stop / make start
```

`make help` lists everything.

---

## Configuration

All configuration is via environment variables in `.env`. See
[`.env.example`](.env.example).

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `RELAY_PUBLIC_URL` | **yes** | — | Public URL of the relay as Nextcloud reaches it. Used to rewrite `download` URLs in `apps.json`. |
| `RELAY_UPSTREAM` | no | `https://apps.nextcloud.com/api/v1` | Upstream app store API. |
| `RELAY_LISTEN` | no | `:8080` | Listen address. |
| `RELAY_CACHE_DIR` | no | `/var/cache/relay` | On-disk cache root (mounted from a docker volume). |
| `RELAY_JSON_TTL` | no | `30m` | How often to re-check the catalog. Refreshes use `If-None-Match`, so a no-op refresh is a single 304 with no body. |
| `RELAY_UPSTREAM_TIMEOUT` | no | `60s` | Timeout for fetching JSON. |
| `RELAY_TARBALL_TIMEOUT` | no | `10m` | Timeout for fetching tarballs. |
| `RELAY_LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error`. |

After editing `.env`, run `make restart`.

---

## Testing the relay

The relay was built and verified against the live upstream. Re-run the
same checks against your deployment.

### a) Catalog reachable and rewritten

```bash
curl -sS http://<relay-ip>:8080/apps.json \
  | jq '[.[].releases[].download] | .[0:3]'
```

Every `download` URL must start with your `RELAY_PUBLIC_URL`.

### b) Install an app through the relay

Admin → Apps → install **Talk** (`spreed`) or another app. The relay log
(`make logs`) shows one `tarball cache miss -> cached`. The Nextcloud
log shows no signature error.

### c) Update flow

Bump an existing app to a newer version. The relay log shows another
cache-miss tarball download.

### d) Verify signature passthrough end-to-end

Pick any app/version and reproduce the exact check Nextcloud performs
(`openssl_verify(tarball, signature, certificate, SHA512)`):

```bash
RELAY=http://<relay-ip>:8080
APP=spreed
VER=9.0.9

curl -sSL "$RELAY/download/$APP/$VER/$APP-$VER.tar.gz" -o /tmp/$APP.tgz
curl -sS "$RELAY/apps.json" | jq -r \
  --arg app "$APP" --arg ver "$VER" '
    .[] | select(.id == $app)
    | { cert: .certificate, sig: (.releases[] | select(.version == $ver) | .signature) }
  ' > /tmp/$APP.meta.json

jq -r .cert /tmp/$APP.meta.json > /tmp/$APP.cert.pem
jq -r .sig  /tmp/$APP.meta.json | base64 -d > /tmp/$APP.sig.bin

openssl x509 -in /tmp/$APP.cert.pem -pubkey -noout -out /tmp/$APP.pub.pem
openssl dgst -sha512 -verify /tmp/$APP.pub.pem \
  -signature /tmp/$APP.sig.bin /tmp/$APP.tgz
# → "Verified OK"
```

If this prints `Verified OK`, Nextcloud will also accept the tarball.

### e) Cache survives a restart

```bash
make restart
time curl -sS -o /dev/null \
  "http://<relay-ip>:8080/download/spreed/9.0.9/spreed-9.0.9.tar.gz"
```

After restart the call is sub-second, served from disk. The relay log
shows `loaded cached endpoint from disk` and `upstream not modified`.

---

## Reverse proxy / HTTPS

The relay speaks plain HTTP. For DMZ-grade exposure, put a TLS terminator
in front of it. Any of these work:

- **Caddy**: `relay.example.org { reverse_proxy 127.0.0.1:8080 }` — done.
- **nginx**: standard `proxy_pass http://127.0.0.1:8080;`.
- **Traefik**: `traefik.http.routers.relay.rule=Host(\`relay.example.org\`)` plus the usual TLS labels.

Whatever you use, set `RELAY_PUBLIC_URL=https://relay.example.org` (no
port if the proxy listens on 443) and `make restart`. Nextcloud will see
HTTPS URLs in the rewritten `apps.json`.

---

## How Nextcloud talks to the relay (reference)

Nextcloud's `AppFetcher` and `CategoryFetcher` build their URL as

```
<appstoreurl>/apps.json
<appstoreurl>/categories.json
```

(no platform suffix, no version header — see
`lib/private/App/AppStore/Fetcher/Fetcher.php::getEndpoint`). So you
point `appstoreurl` at the relay's root and the relay serves those two
paths. Tarball downloads go to `/download/<app>/<version>/<filename>`
on the relay; the rewritten `apps.json` already contains those URLs.

---

## What can go wrong (and how to spot it early)

The relay rides on top of two contracts it doesn't own: Nextcloud's
fetcher code, and the App Store's JSON shape. Both can change. See
[`LIMITATIONS.md`](LIMITATIONS.md) for the full list.

Cheap canary: `make api-check` runs in a few seconds and validates the
upstream shape. Schedule it (or wire it into your monitoring):

```cron
0 * * * *  cd /opt/nextcloud-appstore-relay && make api-check >> /var/log/relay-canary.log 2>&1
```

If it ever exits non-zero, the upstream changed in a way the relay
doesn't handle — read `LIMITATIONS.md` for the matching failure mode.

---

## Building without Docker (optional)

The host doesn't need Go for the standard path — the Docker build
compiles in a multi-stage image. If you want a native binary anyway:

```bash
sudo apt install -y golang-go    # or: brew install go
go build -o bin/relay .
RELAY_PUBLIC_URL=http://127.0.0.1:8080 ./bin/relay
```

## License

MIT — see [`LICENSE`](LICENSE).
