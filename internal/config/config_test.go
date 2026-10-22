package config

import (
	"slices"
	"testing"
	"time"
)

func TestFromEnv(t *testing.T) {
	t.Setenv("PORT", "9000")
	t.Setenv("SHEETSERVE_CACHE_TTL", "90s")
	t.Setenv("SHEETSERVE_CORS_ORIGINS", "https://a.example, https://b.example,")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9000" || c.CacheTTL != 90*time.Second ||
		!slices.Equal(c.CORSOrigins, []string{"https://a.example", "https://b.example"}) {
		t.Errorf("FromEnv: %+v", c)
	}

	t.Setenv("SHEETSERVE_CACHE_TTL", "soon")
	if _, err := FromEnv(); err == nil {
		t.Error("bad SHEETSERVE_CACHE_TTL accepted")
	}
}
