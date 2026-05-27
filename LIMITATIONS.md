# Limitations

This document is intentionally honest about how the relay can fail. The
relay rides on top of two contracts it does not own — Nextcloud's
`AppFetcher` and the App Store's JSON shape — and both can change.

## What it relies on (and what would break it)

### 1. App-tarball signature is computed over the tarball bytes alone

The relay's URL-rewriting in `apps.json` is only safe because the
signature does *not* cover the JSON. Verified against
`lib/private/Installer.php` in `nextcloud/server`:

```php
$verified = openssl_verify(
    file_get_contents($tempFile),
    base64_decode($app['releases'][0]['signature']),
    $certificate,
    OPENSSL_ALGO_SHA512
) === 1;
```

If Nextcloud ever moves to signing the JSON (e.g. a manifest hash that
includes the `download` field), every release would fail signature
verification through the relay. **How you'd notice:** every app install
fails with a signature error in the Nextcloud log, even though the
tarball SHA-512 matches direct download. Fix: stop rewriting URLs and
instead serve `apps.json` verbatim — but then Nextcloud needs direct
egress to GitHub etc., which is the problem this relay solves.

### 2. Nextcloud fetches `<appstoreurl>/apps.json` and `<appstoreurl>/categories.json`

The relay only serves those two paths plus `/download/...`. If
Nextcloud changes its fetcher to call e.g.
`<appstoreurl>/platform/<version>/apps.json`, the relay returns 404 and
Nextcloud sees an empty store. **How you'd notice:** Admin → Apps shows
"App Store not available" or the catalog goes empty; relay logs show
404s for unknown paths.

### 3. Upstream JSON has `id`, `releases[].version`, `releases[].download`

The rewriter parses `apps.json` as schema-tolerant nested maps and only
touches the `download` field. Any field it doesn't know is round-tripped
verbatim through `encoding/json`. If the upstream renames `download`,
nests it deeper, or splits it into multiple URLs (mirrors, deltas), the
rewriter will leave the URL alone and Nextcloud will try to reach the
original host. **How you'd notice:** `rewrote apps.json` log line shows
`releases_indexed` collapsing toward zero, or the rewritten JSON still
contains foreign hosts in `download`. Detect early with:

```bash
curl -sS http://relay/apps.json \
  | jq '[.[].releases[].download | select(startswith("https://YOUR_RELAY") | not)] | length'
```

If that count is > 0, the rewriter missed releases.

### 4. Re-serialization is byte-identical *enough*

Go's `encoding/json` re-marshals the catalog with sorted keys and minimal
whitespace. The bytes differ from the upstream bytes, but the *semantics*
are identical and the signature does not depend on JSON. If a future
Nextcloud version ever does a byte-level integrity check on `apps.json`,
the relay would have to switch to a streaming string-substitution rewrite.
Not implemented today because nothing currently asks for it.

## What it does not do

- **No HTTPS termination.** Run it behind a reverse proxy (Caddy, nginx,
  Traefik) that adds TLS and forwards to `:8080`. Same proxy can also
  add basic auth if you want to gate the relay.
- **No authentication of clients.** Anyone who can reach the relay can
  download tarballs through it. In a DMZ this is normally fine because
  only Nextcloud reaches it.
- **No content filtering / allowlist.** The relay will fetch any URL
  that appears in the upstream `apps.json` `download` field. If you don't
  trust the upstream catalog, this relay is the wrong tool — and
  Nextcloud's own signature check is your real defense anyway.
- **No singleflight on simultaneous cache misses.** Two parallel requests
  for the same uncached tarball both fetch upstream; the last one to
  finish wins the cache slot. Wastes bandwidth, doesn't corrupt anything.
- **No HTTP range / resume on tarball downloads.** The relay always
  serves the full file. Nextcloud doesn't use range requests for app
  installs, so this hasn't mattered.
- **No catalog filtering by Nextcloud platform version.** The full
  upstream catalog is forwarded; Nextcloud filters client-side using
  `platformVersionSpec`, exactly as it does against the real store.

## How to detect a silent break

The relay is "silently correct" most of the time. Two cheap canaries:

1. **`releases_indexed` in logs.** Should be roughly stable
   (low-thousands today). A sudden drop = upstream schema shift.
2. **End-to-end signature check.** The script in
   [`README.md` § Testing the relay (d)](README.md) reproduces
   Nextcloud's `openssl_verify` call. Run it from cron against a
   well-known app (e.g. `spreed` latest); if it ever prints anything
   other than `Verified OK`, page yourself.
