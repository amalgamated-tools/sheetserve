package config

import (
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amalgamated-tools/sheetserve/internal/store"
)

// setRequired sets every required variable to a valid value.
func setRequired(t *testing.T) {
	for k, v := range map[string]string{
		"SHEETSERVE_BASE_URL":   "https://sheets.example",
		"SHEETSERVE_SECRET":     base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"GOOGLE_CLIENT_ID":      "123-x.apps.googleusercontent.com",
		"GOOGLE_CLIENT_SECRET":  "s",
		"GOOGLE_PICKER_API_KEY": "k",
		"GOOGLE_PROJECT_NUMBER": "123",
	} {
		t.Setenv(k, v)
	}
}

func TestFromEnv(t *testing.T) {
	setRequired(t)
	t.Setenv("PORT", "9000")
	t.Setenv("SHEETSERVE_CACHE_TTL", "90s")
	t.Setenv("SHEETSERVE_CORS_ORIGINS", "https://a.example, https://b.example,")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 9000 || c.CacheTTL != 90*time.Second || len(c.DecryptedSecret) != 32 ||
		!slices.Equal(c.CORSOrigins, []string{"https://a.example", "https://b.example"}) {
		t.Errorf("FromEnv: %+v", c)
	}

	t.Setenv("SHEETSERVE_CACHE_TTL", "soon")
	if _, err := FromEnv(); err == nil {
		t.Error("bad SHEETSERVE_CACHE_TTL accepted")
	}
	t.Setenv("SHEETSERVE_CACHE_TTL", "")
	for _, port := range []string{"http", "0", "70000"} {
		t.Setenv("PORT", port)
		if _, err := FromEnv(); err == nil {
			t.Errorf("bad PORT %q accepted", port)
		}
	}
	t.Setenv("PORT", "")
	if c, err := FromEnv(); err != nil || c.Port != 8085 {
		t.Errorf("default port: %v %v", c, err)
	}

	// The Google quotas default to the Sheets API's own, and 0 turns one off.
	if c, err = FromEnv(); err != nil {
		t.Fatal(err)
	}
	if c.ProjectQuota != (store.Quota{ReadsPerMinute: 300, WritesPerMinute: 300}) ||
		c.UserQuota != (store.Quota{ReadsPerMinute: 60, WritesPerMinute: 60}) || c.KeyQuota != (store.Quota{ReadsPerMinute: 30, WritesPerMinute: 30}) {
		t.Errorf("default quotas: %+v %+v %+v", c.ProjectQuota, c.UserQuota, c.KeyQuota)
	}
	t.Setenv("SHEETSERVE_GOOGLE_READS_PER_MINUTE", "600")
	t.Setenv("SHEETSERVE_GOOGLE_KEY_WRITES_PER_MINUTE", "0")
	if c, err = FromEnv(); err != nil {
		t.Fatal(err)
	}
	if c.ProjectQuota.ReadsPerMinute != 600 || c.KeyQuota.WritesPerMinute != 0 {
		t.Errorf("set quotas: %+v %+v", c.ProjectQuota, c.KeyQuota)
	}
	for _, v := range []string{"lots", "-1"} {
		t.Setenv("SHEETSERVE_GOOGLE_USER_READS_PER_MINUTE", v)
		if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "SHEETSERVE_GOOGLE_USER_READS_PER_MINUTE") {
			t.Errorf("bad quota %q: %v", v, err)
		}
	}
}

// On Render, the base URL defaults to the service's public URL.
func TestBaseURLFromRender(t *testing.T) {
	setRequired(t)
	t.Setenv("SHEETSERVE_BASE_URL", "")
	t.Setenv("RENDER_EXTERNAL_URL", "https://sheetserve.onrender.com")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "https://sheetserve.onrender.com" {
		t.Errorf("BaseURL %q", c.BaseURL)
	}
}

// A project ID in GOOGLE_PROJECT_NUMBER fails at startup instead of breaking every pick.
func TestRejectsProjectID(t *testing.T) {
	setRequired(t)
	t.Setenv("GOOGLE_PROJECT_NUMBER", "my-project-512320")
	if _, err := FromEnv(); !errors.Is(err, ErrInvalidGoogleProjectNumber) {
		t.Errorf("got %v", err)
	}
}

// A preview names the relay it signs in through; the relay lists the origins it serves.
func TestRelay(t *testing.T) {
	setRequired(t)
	t.Setenv("SHEETSERVE_AUTH_RELAY", "https://sheets.example/auth/relay")
	t.Setenv("SHEETSERVE_RELAY_ORIGINS", "https://sheetserve-pr-*.onrender.com, http://localhost:*")
	c, err := FromEnv()
	if err != nil || c.AuthRelay != "https://sheets.example/auth/relay" ||
		!slices.Equal(c.RelayOrigins, []string{"https://sheetserve-pr-*.onrender.com", "http://localhost:*"}) {
		t.Fatalf("relay: %+v %v", c, err)
	}
	for _, o := range []string{"sheetserve-pr-*.onrender.com", "ftp://a.example", "https://a.example/", "https://[a", "https://"} {
		t.Setenv("SHEETSERVE_RELAY_ORIGINS", o)
		if _, err := FromEnv(); err == nil {
			t.Errorf("bad relay origin %q accepted", o)
		}
	}
	t.Setenv("SHEETSERVE_RELAY_ORIGINS", "")
	t.Setenv("SHEETSERVE_AUTH_RELAY", "/auth/relay")
	if _, err := FromEnv(); err == nil {
		t.Error("relative SHEETSERVE_AUTH_RELAY accepted")
	}
}
