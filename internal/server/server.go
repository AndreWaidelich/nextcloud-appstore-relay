package server

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/AndreWaidelich/nextcloud-appstore-relay/internal/catalog"
	"github.com/AndreWaidelich/nextcloud-appstore-relay/internal/tarball"
)

type Server struct {
	catalog *catalog.Manager
	tarball *tarball.Store
	log     *slog.Logger
}

func New(cat *catalog.Manager, tb *tarball.Store, log *slog.Logger) *Server {
	return &Server{catalog: cat, tarball: tb, log: log}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /apps.json", s.handleApps)
	mux.HandleFunc("HEAD /apps.json", s.handleApps)
	mux.HandleFunc("GET /categories.json", s.handleCategories)
	mux.HandleFunc("HEAD /categories.json", s.handleCategories)
	mux.HandleFunc("GET /download/{app}/{version}/{filename}", s.handleDownload)
	mux.HandleFunc("HEAD /download/{app}/{version}/{filename}", s.handleDownload)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /", s.handleRoot)
	return logMiddleware(s.log, mux)
}

func (s *Server) handleApps(w http.ResponseWriter, r *http.Request) {
	body, err := s.catalog.Apps()
	if err != nil {
		http.Error(w, "catalog not yet available", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Relay-Catalog-Age-Seconds", durationSecs(s.catalog.AgeApps()))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}

func (s *Server) handleCategories(w http.ResponseWriter, r *http.Request) {
	body, err := s.catalog.Categories()
	if err != nil {
		http.Error(w, "categories not yet available", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	version := r.PathValue("version")
	if app == "" || version == "" {
		http.Error(w, "missing app/version", http.StatusBadRequest)
		return
	}
	origURL, ok := s.catalog.Lookup(app, version)
	if !ok {
		s.log.Warn("download for unknown (app, version)", "app", app, "version", version)
		http.Error(w, "app/version not in current catalog", http.StatusNotFound)
		return
	}
	s.tarball.Serve(r.Context(), w, r, app, version, origURL)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if _, err := s.catalog.Apps(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("catalog: not ready\n"))
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(
		"nextcloud-appstore-relay\n\n" +
			"Endpoints:\n" +
			"  GET /apps.json\n" +
			"  GET /categories.json\n" +
			"  GET /download/<app>/<version>/<filename>.tar.gz\n" +
			"  GET /healthz\n",
	))
}

func durationSecs(d time.Duration) string {
	if d < 0 {
		return "-1"
	}
	s := int64(d / time.Second)
	if s < 0 {
		s = 0
	}
	return formatInt(s)
}

func formatInt(n int64) string {
	var sb strings.Builder
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		sb.WriteByte('-')
	}
	sb.Write(digits[i:])
	return sb.String()
}

// logMiddleware records one structured line per request with method, path,
// status and bytes — enough to trace a Nextcloud install through the relay.
func logMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &recordingWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)
		log.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"bytes", rw.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
}

type recordingWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (rw *recordingWriter) WriteHeader(code int) {
	if !rw.wrote {
		rw.status = code
		rw.wrote = true
		rw.ResponseWriter.WriteHeader(code)
	}
}

func (rw *recordingWriter) Write(b []byte) (int, error) {
	if !rw.wrote {
		rw.wrote = true
	}
	n, err := rw.ResponseWriter.Write(b)
	rw.bytes += int64(n)
	return n, err
}

// Flush is exposed so streaming responses (large tarballs) can flush headers
// promptly without buffering.
func (rw *recordingWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
