# nextcloud-appstore-relay

> Ein schlanker Relay, der einer abgeschotteten Nextcloud-Instanz erlaubt,
> Apps aus dem offiziellen App Store zu installieren und zu aktualisieren —
> **ohne** dass die Nextcloud selbst ins Internet darf.

Der Relay läuft in der DMZ, spricht stellvertretend mit
`apps.nextcloud.com` und den Tarball-Hosts (GitHub etc.), und liefert
Nextcloud eine byte-identische Sicht auf Katalog und Pakete.

```
   ┌─────────────────┐         ┌────────────────────┐         ┌──────────────────┐
   │   Nextcloud     │  HTTP   │       Relay        │  HTTPS  │ apps.nextcloud   │
   │  (kein Outbound)│ ──────► │      (DMZ)         │ ──────► │ github.com, ...  │
   └─────────────────┘         │                    │         └──────────────────┘
                               │ - Rewrite apps.json│
                               │ - Stream Tarballs  │
                               │ - Disk-Cache       │
                               └────────────────────┘
```

Der Relay tut **genau zwei Dinge**, sonst nichts:

1. **Schreibt die `download`-URL** jeder App-Release in `apps.json` so um,
   dass sie auf den Relay zeigt. Nextcloud — die sonst direkt zu
   `github.com` greifen würde — holt das Tarball also über den Relay.
2. **Streamt das Original-Tarball byte-genau durch.** Keine Dekompression,
   keine Re-Archivierung. Nextclouds Signaturprüfung (`openssl_verify`
   mit SHA-512 gegen das Zertifikat aus dem Katalog) bleibt damit
   unverändert gültig — Details siehe [`LIMITATIONS.md`](LIMITATIONS.md).

Alles Weitere (Caching, Logging, Conditional GETs) ist nur
Implementierungsdetail.

---

## Inhaltsverzeichnis

- [Quick Start (Ubuntu / Debian)](#quick-start-ubuntu--debian)
- [Tagesbetrieb](#tagesbetrieb)
- [Konfiguration](#konfiguration)
- [Funktionstest](#funktionstest)
- [Reverse Proxy / HTTPS](#reverse-proxy--https)
- [Wie Nextcloud den Relay anspricht](#wie-nextcloud-den-relay-anspricht-referenz)
- [Was kaputt gehen kann](#was-kaputt-gehen-kann-und-wie-mans-merkt)
- [Build ohne Docker (optional)](#build-ohne-docker-optional)
- [Lizenz](#lizenz)

---

## Quick Start (Ubuntu / Debian)

Drei Zeilen auf dem Host, der den Relay laufen lassen soll:

```bash
git clone https://github.com/AndreWaidelich/nextcloud-appstore-relay.git
cd nextcloud-appstore-relay
sudo make install
```

Der Installer (`scripts/install.sh`) ist idempotent und macht:

1. Prüft, dass es ein Ubuntu/Debian ist.
2. Installiert fehlende Pakete: `curl`, `jq`, `docker.io`, `docker-compose-v2`.
3. Fragt nach `RELAY_PUBLIC_URL` (die URL, unter der deine Nextcloud den
   Relay erreicht). Default: `http://<host-ip>:8080`.
4. Schreibt `.env`.
5. Führt den Upstream-API-Check aus (`make api-check`).
6. `docker compose up -d --build`.
7. Wartet bis `/healthz` antwortet und druckt den `occ`-Befehl, den du
   noch auf der Nextcloud-Seite ausführen musst.

Danach **einmal auf der Nextcloud** (nicht hier):

```bash
sudo -u www-data php occ config:system:set appstoreurl \
    --value="http://<relay-ip>:8080"
```

Fertig. Admin → Apps installieren oder updaten. `make logs` zeigt im
Idealfall:

```
"tarball cache miss -> cached"   # erster Install
"tarball cache hit"              # alle weiteren
```

### Non-interaktive Installation

```bash
sudo make install-yes                                          # Default: Host-IP:8080
sudo bash scripts/install.sh --public-url http://10.0.0.5:8080 --yes
```

---

## Tagesbetrieb

| Befehl              | Zweck                                                          |
|---------------------|----------------------------------------------------------------|
| `make logs`         | Container-Logs folgen                                          |
| `make status`       | Container-Status + Cache-Größe auf Disk                        |
| `make api-check`    | Canary: stimmt das Upstream-API-Schema noch?                   |
| `make restart`      | Änderungen aus `.env` anwenden                                 |
| `make update`       | `git pull` + neu bauen + neu starten                           |
| `make stop` / `make start` | Container anhalten / starten                            |
| `make clean-cache`  | Disk-Cache wegwerfen (Relay holt alles wieder neu)             |
| `make help`         | Alle Targets auflisten                                         |

---

## Konfiguration

Alles über Umgebungsvariablen in `.env`. Vorlage:
[`.env.example`](.env.example).

| Variable                  | Pflicht | Default                                  | Bedeutung |
|---------------------------|:-------:|------------------------------------------|-----------|
| `RELAY_PUBLIC_URL`        | **ja**  | —                                        | URL, unter der deine Nextcloud den Relay erreicht. Wird in `apps.json` als neue `download`-Basis eingesetzt. |
| `RELAY_UPSTREAM`          | nein    | `https://apps.nextcloud.com/api/v1`      | Quell-Store. |
| `RELAY_LISTEN`            | nein    | `:8080`                                  | Listen-Adresse. |
| `RELAY_CACHE_DIR`         | nein    | `/var/cache/relay`                       | Disk-Cache-Pfad (per Docker-Volume gemountet). |
| `RELAY_JSON_TTL`          | nein    | `30m`                                    | Wie oft der Katalog gegen den Upstream geprüft wird. Re-Checks nutzen `If-None-Match` — wenn sich nichts geändert hat, ein 304 ohne Body, also billig. |
| `RELAY_UPSTREAM_TIMEOUT`  | nein    | `60s`                                    | Timeout für JSON-Fetch. |
| `RELAY_TARBALL_TIMEOUT`   | nein    | `10m`                                    | Timeout für Tarball-Downloads. |
| `RELAY_LOG_LEVEL`         | nein    | `info`                                   | `debug` · `info` · `warn` · `error`. |

Nach Änderungen in `.env`: `make restart`.

---

## Funktionstest

Der Relay wurde gegen die Live-API verifiziert. Du kannst die gleichen
Checks gegen dein Deployment wiederholen.

### a) Katalog erreichbar und umgeschrieben

```bash
curl -sS http://<relay-ip>:8080/apps.json \
  | jq '[.[].releases[].download] | .[0:3]'
```

Jede `download`-URL muss mit deinem `RELAY_PUBLIC_URL` beginnen.

### b) App installieren

Admin → Apps → z. B. **Talk** (`spreed`) installieren. Im Relay-Log
erscheint einmal `tarball cache miss -> cached`, in Nextclouds Log
keinerlei Signaturfehler.

### c) Update durchspielen

Eine bereits installierte App auf eine neuere Version aktualisieren —
zweiter Cache-Miss-Eintrag.

### d) Signatur durchgereicht (End-to-End)

Reproduziert exakt den Check, den Nextcloud intern macht:
`openssl_verify(tarball, signature, certificate, SHA-512)`.

```bash
RELAY=http://<relay-ip>:8080
APP=spreed
VER=9.0.9

# 1) Tarball über den Relay laden
curl -sSL "$RELAY/download/$APP/$VER/$APP-$VER.tar.gz" -o /tmp/$APP.tgz

# 2) Signatur + Zertifikat aus dem (umgeschriebenen) apps.json ziehen
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
# -> "Verified OK"
```

Gibt das `Verified OK` aus, wird auch Nextcloud das Tarball akzeptieren.
Sonst stimmt was mit dem Byte-Stream nicht — als erstes mit dem
Direkt-Download per SHA-512 vergleichen.

### e) Cache überlebt Neustart

```bash
make restart
time curl -sS -o /dev/null \
  "http://<relay-ip>:8080/download/spreed/9.0.9/spreed-9.0.9.tar.gz"
```

Sub-Sekunde nach Restart, Logs zeigen `loaded cached endpoint from disk`
und `upstream not modified`.

---

## Reverse Proxy / HTTPS

Der Relay spricht reines HTTP. Für DMZ-Betrieb gehört ein TLS-Terminator
davor. Drei gängige Varianten:

```caddy
# Caddyfile
relay.example.org {
    reverse_proxy 127.0.0.1:8080
}
```

```nginx
# nginx
server {
    listen 443 ssl http2;
    server_name relay.example.org;
    ssl_certificate     /etc/letsencrypt/live/relay.example.org/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/relay.example.org/privkey.pem;
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
    }
}
```

```yaml
# Traefik (Labels am Relay-Service)
labels:
  - "traefik.http.routers.relay.rule=Host(`relay.example.org`)"
  - "traefik.http.routers.relay.tls.certresolver=le"
  - "traefik.http.services.relay.loadbalancer.server.port=8080"
```

In `.env` dann `RELAY_PUBLIC_URL=https://relay.example.org` setzen und
`make restart` — Nextcloud sieht ab dann HTTPS-URLs im umgeschriebenen
`apps.json`.

---

## Wie Nextcloud den Relay anspricht (Referenz)

Nextclouds `AppFetcher` und `CategoryFetcher` bauen ihre URL als:

```
<appstoreurl>/apps.json
<appstoreurl>/categories.json
```

(kein Platform-Suffix, kein Version-Header — siehe
`lib/private/App/AppStore/Fetcher/Fetcher.php::getEndpoint`).

`appstoreurl` zeigt also auf die Relay-Root, und der Relay liefert genau
diese zwei Pfade. Tarball-Downloads gehen an
`/download/<app>/<version>/<filename>` — diese URLs stehen schon in der
umgeschriebenen `apps.json`.

---

## Was kaputt gehen kann (und wie man's merkt)

Der Relay stützt sich auf zwei Verträge, die ihm nicht gehören: Nextclouds
Fetcher-Code und das JSON-Schema des App Stores. Beide können sich
ändern. Vollständige Liste in [`LIMITATIONS.md`](LIMITATIONS.md).

Billiger Canary: `make api-check` läuft in Sekunden und prüft das
Upstream-Schema. Ideal als Cronjob:

```cron
0 * * * *  cd /opt/nextcloud-appstore-relay && make api-check >> /var/log/relay-canary.log 2>&1
```

Exit-Code ≠ 0 heißt: Upstream hat sich verändert. Welche Bruchstelle das
ist, steht in `LIMITATIONS.md`.

---

## Build ohne Docker (optional)

Der Standardweg läuft komplett in Docker — der Host braucht **kein** Go,
der Multi-Stage-Build kompiliert intern. Wenn du trotzdem ein natives
Binary willst:

```bash
sudo apt install -y golang-go
go build -o bin/relay .
RELAY_PUBLIC_URL=http://127.0.0.1:8080 ./bin/relay
```

---

## Lizenz

MIT — siehe [`LICENSE`](LICENSE).
