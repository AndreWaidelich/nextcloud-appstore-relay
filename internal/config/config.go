package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

type Config struct {
	Listen          string
	Upstream        string
	PublicURL       string
	CacheDir        string
	JSONTTL         time.Duration
	UpstreamTimeout time.Duration
	TarballTimeout  time.Duration
	LogLevel        string
}

func FromEnv() (Config, error) {
	c := Config{
		Listen:          getenv("RELAY_LISTEN", ":8080"),
		Upstream:        strings.TrimRight(getenv("RELAY_UPSTREAM", "https://apps.nextcloud.com/api/v1"), "/"),
		PublicURL:       strings.TrimRight(os.Getenv("RELAY_PUBLIC_URL"), "/"),
		CacheDir:        getenv("RELAY_CACHE_DIR", "/var/cache/relay"),
		JSONTTL:         mustDuration("RELAY_JSON_TTL", 30*time.Minute),
		UpstreamTimeout: mustDuration("RELAY_UPSTREAM_TIMEOUT", 60*time.Second),
		TarballTimeout:  mustDuration("RELAY_TARBALL_TIMEOUT", 10*time.Minute),
		LogLevel:        getenv("RELAY_LOG_LEVEL", "info"),
	}
	if c.PublicURL == "" {
		return c, errors.New("RELAY_PUBLIC_URL is required (e.g. https://relay.example)")
	}
	if u, err := url.Parse(c.PublicURL); err != nil || u.Scheme == "" || u.Host == "" {
		return c, fmt.Errorf("RELAY_PUBLIC_URL %q is not a valid absolute URL", c.PublicURL)
	}
	if u, err := url.Parse(c.Upstream); err != nil || u.Scheme == "" || u.Host == "" {
		return c, fmt.Errorf("RELAY_UPSTREAM %q is not a valid absolute URL", c.Upstream)
	}
	return c, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mustDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: env %s=%q is not a valid duration, using default %s\n", key, v, def)
		return def
	}
	return d
}
