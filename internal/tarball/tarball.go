package tarball

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Store struct {
	dir   string
	httpc *http.Client
	log   *slog.Logger
}

func New(cacheDir string, timeout time.Duration, log *slog.Logger) (*Store, error) {
	dir := filepath.Join(cacheDir, "tarballs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{
		dir:   dir,
		httpc: &http.Client{Timeout: timeout},
		log:   log,
	}, nil
}

func (s *Store) path(appID, version string) string {
	return filepath.Join(s.dir, appID, version+".tar.gz")
}

// Serve writes the tarball for (appID, version) to w. On cache hit it streams
// from disk. On cache miss it fetches from origURL, simultaneously streaming
// to the client and persisting to disk (atomic temp+rename). The tarball is
// passed through byte-for-byte: no decompression, no re-encoding.
func (s *Store) Serve(ctx context.Context, w http.ResponseWriter, r *http.Request, appID, version, origURL string) {
	cachePath := s.path(appID, version)

	if f, err := os.Open(cachePath); err == nil {
		defer f.Close()
		fi, err := f.Stat()
		if err == nil {
			s.setHeaders(w, fi.Size(), fi.ModTime())
			s.log.Info("tarball cache hit", "app", appID, "version", version, "bytes", fi.Size())
			if r.Method == http.MethodHead {
				return
			}
			http.ServeContent(w, r, filepath.Base(cachePath), fi.ModTime(), f)
			return
		}
	}

	// Cache miss: fetch upstream.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origURL, nil)
	if err != nil {
		http.Error(w, "bad upstream URL", http.StatusInternalServerError)
		return
	}
	req.Header.Set("User-Agent", "nextcloud-appstore-relay/0.1")
	req.Header.Set("Accept", "application/octet-stream")

	start := time.Now()
	resp, err := s.httpc.Do(req)
	if err != nil {
		s.log.Error("upstream tarball fetch failed", "app", appID, "version", version, "url", origURL, "err", err.Error())
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		s.log.Error("upstream tarball non-200", "app", appID, "version", version, "url", origURL, "status", resp.StatusCode)
		http.Error(w, fmt.Sprintf("upstream returned %d", resp.StatusCode), http.StatusBadGateway)
		return
	}

	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		s.log.Error("mkdir cache subdir failed", "err", err.Error())
		http.Error(w, "cache error", http.StatusInternalServerError)
		return
	}

	tmp, err := os.CreateTemp(filepath.Dir(cachePath), ".tmp-*")
	if err != nil {
		s.log.Error("create temp cache file failed", "err", err.Error())
		http.Error(w, "cache error", http.StatusInternalServerError)
		return
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpName)
		}
	}()

	// Forward upstream length if known so NC can show progress and verify size.
	if resp.ContentLength > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	w.Header().Set("Content-Type", "application/gzip")
	// Tarballs are immutable per version; let intermediate caches keep them.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)

	mw := io.MultiWriter(w, tmp)
	n, copyErr := io.Copy(mw, resp.Body)

	if copyErr != nil {
		tmp.Close()
		s.log.Warn("tarball stream interrupted (not cached)",
			"app", appID, "version", version, "url", origURL, "bytes", n, "err", copyErr.Error())
		return // cleanup=true removes the temp file
	}
	if resp.ContentLength > 0 && n != resp.ContentLength {
		tmp.Close()
		s.log.Warn("tarball short read (not cached)",
			"app", appID, "version", version, "got", n, "expected", resp.ContentLength)
		return
	}

	if err := tmp.Sync(); err != nil {
		tmp.Close()
		s.log.Warn("tarball sync failed (not cached)", "err", err.Error())
		return
	}
	if err := tmp.Close(); err != nil {
		s.log.Warn("tarball close failed (not cached)", "err", err.Error())
		return
	}
	if err := os.Rename(tmpName, cachePath); err != nil {
		s.log.Warn("tarball rename into cache failed", "err", err.Error())
		return
	}
	cleanup = false

	s.log.Info("tarball cache miss -> cached",
		"app", appID, "version", version, "bytes", n,
		"upstream_status", resp.StatusCode,
		"duration_ms", time.Since(start).Milliseconds(),
	)
}

func (s *Store) setHeaders(w http.ResponseWriter, size int64, mtime time.Time) {
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
}

// Errors that callers may want to surface explicitly.
var (
	ErrNotInIndex = errors.New("app/version not in current catalog")
)
