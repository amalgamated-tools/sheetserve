package config

import (
	"cmp"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config is the server-wide settings. Each user's spreadsheets and keys are db.Settings.
type Config struct {
	Listen      string
	CacheTTL    time.Duration
	CORSOrigins []string
}

// FromEnv reads the server-wide settings: PORT, SHEETSERVE_CACHE_TTL and SHEETSERVE_CORS_ORIGINS.
func FromEnv() (*Config, error) {
	c := Config{Listen: ":" + cmp.Or(os.Getenv("PORT"), "8080")}
	if ttl := os.Getenv("SHEETSERVE_CACHE_TTL"); ttl != "" {
		d, err := time.ParseDuration(ttl)
		if err != nil || d < 0 {
			return nil, fmt.Errorf("SHEETSERVE_CACHE_TTL: want a duration such as 60s, got %q", ttl)
		}
		c.CacheTTL = d
	}
	for o := range strings.SplitSeq(os.Getenv("SHEETSERVE_CORS_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.CORSOrigins = append(c.CORSOrigins, o)
		}
	}
	return &c, nil
}
