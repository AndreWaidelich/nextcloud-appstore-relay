package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	appsFile       = "apps.json"
	categoriesFile = "categories.json"

	rawAppsCache       = "upstream-apps.json"
	rawCategoriesCache = "upstream-categories.json"
	metaSuffix         = ".meta.json"
)

type Manager struct {
	upstream  string
	publicURL string
	cacheDir  string
	ttl       time.Duration
	httpc     *http.Client
	log       *slog.Logger

	mu          sync.RWMutex
	apps        endpointState
	categories  endpointState
	rewritten   []byte
	downloadIdx map[string]string // "<app>|<version>" -> original URL
}

type endpointState struct {
	body         []byte
	etag         string
	lastModified string
	fetchedAt    time.Time
	upstreamURL  string
}

type metaFile struct {
	ETag         string    `json:"etag"`
	LastModified string    `json:"last_modified"`
	FetchedAt    time.Time `json:"fetched_at"`
	UpstreamURL  string    `json:"upstream_url"`
}

func New(upstream, publicURL, cacheDir string, ttl, timeout time.Duration, log *slog.Logger) (*Manager, error) {
	if err := os.MkdirAll(filepath.Join(cacheDir, "meta"), 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	m := &Manager{
		upstream:  upstream,
		publicURL: publicURL,
		cacheDir:  cacheDir,
		ttl:       ttl,
		httpc:     &http.Client{Timeout: timeout},
		log:       log,
	}
	m.apps.upstreamURL = upstream + "/" + appsFile
	m.categories.upstreamURL = upstream + "/" + categoriesFile
	m.loadFromDisk()
	return m, nil
}

// Start launches a background refresher that re-fetches the catalog every TTL.
// Returns immediately. The refresher exits when ctx is cancelled.
func (m *Manager) Start(ctx context.Context) {
	// Kick an initial refresh synchronously (best-effort) so requests served
	// immediately after startup don't return 503 if the disk cache is cold.
	_ = m.Refresh(ctx)

	go func() {
		t := time.NewTicker(m.ttl)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = m.Refresh(ctx)
			}
		}
	}()
}

// Refresh re-fetches both endpoints with conditional GET. Errors are logged
// but do not clear existing state; the relay keeps serving stale on failure.
func (m *Manager) Refresh(ctx context.Context) error {
	var firstErr error
	if err := m.refreshOne(ctx, &m.apps, rawAppsCache, true); err != nil {
		firstErr = err
	}
	if err := m.refreshOne(ctx, &m.categories, rawCategoriesCache, false); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (m *Manager) refreshOne(ctx context.Context, st *endpointState, fname string, isApps bool) error {
	m.mu.RLock()
	prevEtag := st.etag
	prevLM := st.lastModified
	url := st.upstreamURL
	m.mu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "nextcloud-appstore-relay/0.1 (+https://github.com/AndreWaidelich/nextcloud-appstore-relay)")
	req.Header.Set("Accept", "application/json")
	if prevEtag != "" {
		req.Header.Set("If-None-Match", prevEtag)
	}
	if prevLM != "" {
		req.Header.Set("If-Modified-Since", prevLM)
	}

	resp, err := m.httpc.Do(req)
	if err != nil {
		m.log.Warn("upstream fetch failed (serving stale)", "url", url, "err", err.Error())
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		m.mu.Lock()
		st.fetchedAt = time.Now()
		m.mu.Unlock()
		m.writeMeta(fname, st)
		m.log.Info("upstream not modified", "url", url, "etag", prevEtag)
		return nil
	case http.StatusOK:
		// proceed
	default:
		err := fmt.Errorf("upstream %s returned HTTP %d", url, resp.StatusCode)
		m.log.Warn("unexpected upstream status (serving stale)", "url", url, "status", resp.StatusCode)
		return err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read upstream body: %w", err)
	}

	if err := m.writeRaw(fname, body); err != nil {
		m.log.Warn("could not persist raw cache", "file", fname, "err", err.Error())
	}

	m.mu.Lock()
	st.body = body
	st.etag = resp.Header.Get("ETag")
	st.lastModified = resp.Header.Get("Last-Modified")
	st.fetchedAt = time.Now()
	m.mu.Unlock()
	m.writeMeta(fname, st)

	if isApps {
		if err := m.rebuildRewritten(); err != nil {
			m.log.Error("rewrite failed", "err", err.Error())
			return err
		}
	}

	m.log.Info("upstream refreshed",
		"url", url, "bytes", len(body), "etag", resp.Header.Get("ETag"))
	return nil
}

// rebuildRewritten parses the cached apps.json, replaces every release's
// "download" field with a relay URL, and stores the result. It also rebuilds
// the (app, version) -> original URL lookup index.
func (m *Manager) rebuildRewritten() error {
	m.mu.RLock()
	raw := m.apps.body
	m.mu.RUnlock()
	if len(raw) == 0 {
		return errors.New("no apps.json body to rewrite")
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var apps []map[string]any
	if err := dec.Decode(&apps); err != nil {
		return fmt.Errorf("parse apps.json: %w", err)
	}

	idx := make(map[string]string, len(apps)*4)
	var skipped int

	for _, app := range apps {
		appID, _ := app["id"].(string)
		releases, _ := app["releases"].([]any)
		for _, r := range releases {
			rel, ok := r.(map[string]any)
			if !ok {
				continue
			}
			version, _ := rel["version"].(string)
			origURL, _ := rel["download"].(string)
			if appID == "" || version == "" || origURL == "" {
				continue
			}
			if !safeSegment(appID) || !safeSegment(version) {
				skipped++
				continue
			}
			key := appID + "|" + version
			idx[key] = origURL
			rel["download"] = m.buildRelayURL(appID, version)
		}
	}

	// Re-marshal. json.Marshal sorts map keys alphabetically — that changes
	// byte structure, which is fine: the signature does not cover JSON.
	out, err := json.Marshal(apps)
	if err != nil {
		return fmt.Errorf("marshal rewritten apps.json: %w", err)
	}

	m.mu.Lock()
	m.rewritten = out
	m.downloadIdx = idx
	m.mu.Unlock()

	m.log.Info("rewrote apps.json", "releases_indexed", len(idx), "releases_skipped", skipped, "bytes_in", len(raw), "bytes_out", len(out))
	return nil
}

func (m *Manager) buildRelayURL(appID, version string) string {
	return fmt.Sprintf("%s/download/%s/%s/%s-%s.tar.gz",
		m.publicURL,
		url.PathEscape(appID),
		url.PathEscape(version),
		url.PathEscape(appID),
		url.PathEscape(version),
	)
}

// safeSegment restricts identifiers used in the relay URL path to a
// conservative set, so we don't have to worry about slashes, query strings or
// percent-decoding subtleties on either side of the wire.
func safeSegment(s string) bool {
	if s == "" || len(s) > 200 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-' || r == '+' || r == '~':
		default:
			return false
		}
	}
	return true
}

// Apps returns the rewritten apps.json body.
func (m *Manager) Apps() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.rewritten) == 0 {
		return nil, errors.New("apps.json not yet available")
	}
	return m.rewritten, nil
}

// Categories returns the categories.json body verbatim.
func (m *Manager) Categories() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.categories.body) == 0 {
		return nil, errors.New("categories.json not yet available")
	}
	return m.categories.body, nil
}

// Lookup returns the original (upstream) download URL for an (app, version)
// pair as seen in the most recent apps.json refresh.
func (m *Manager) Lookup(appID, version string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.downloadIdx[appID+"|"+version]
	return u, ok
}

// AgeApps returns how long ago the apps.json was last successfully checked
// against the upstream (regardless of whether the upstream replied 200 or 304).
func (m *Manager) AgeApps() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.apps.fetchedAt.IsZero() {
		return -1
	}
	return time.Since(m.apps.fetchedAt)
}

// ---- disk cache helpers ----

func (m *Manager) rawPath(fname string) string  { return filepath.Join(m.cacheDir, fname) }
func (m *Manager) metaPath(fname string) string { return filepath.Join(m.cacheDir, "meta", fname+metaSuffix) }

func (m *Manager) writeRaw(fname string, body []byte) error {
	return writeAtomic(m.rawPath(fname), body)
}

func (m *Manager) writeMeta(fname string, st *endpointState) {
	m.mu.RLock()
	meta := metaFile{
		ETag:         st.etag,
		LastModified: st.lastModified,
		FetchedAt:    st.fetchedAt,
		UpstreamURL:  st.upstreamURL,
	}
	m.mu.RUnlock()
	data, err := json.Marshal(meta)
	if err != nil {
		return
	}
	_ = writeAtomic(m.metaPath(fname), data)
}

func (m *Manager) loadFromDisk() {
	m.loadOne(&m.apps, rawAppsCache)
	m.loadOne(&m.categories, rawCategoriesCache)
	if len(m.apps.body) > 0 {
		if err := m.rebuildRewritten(); err != nil {
			m.log.Warn("could not rewrite cached apps.json on startup", "err", err.Error())
		}
	}
}

func (m *Manager) loadOne(st *endpointState, fname string) {
	body, err := os.ReadFile(m.rawPath(fname))
	if err != nil {
		return
	}
	st.body = body
	if mbody, err := os.ReadFile(m.metaPath(fname)); err == nil {
		var meta metaFile
		if json.Unmarshal(mbody, &meta) == nil {
			st.etag = meta.ETag
			st.lastModified = meta.LastModified
			st.fetchedAt = meta.FetchedAt
		}
	}
	m.log.Info("loaded cached endpoint from disk", "file", fname, "bytes", len(body), "etag", st.etag)
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}
