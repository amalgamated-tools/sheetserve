package config

import (
	"encoding/base64"
	"errors"
	"slices"
	"testing"
	"time"
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
	if c.Listen != "9000" || c.CacheTTL != 90*time.Second || len(c.DecryptedSecret) != 32 ||
		!slices.Equal(c.CORSOrigins, []string{"https://a.example", "https://b.example"}) {
		t.Errorf("FromEnv: %+v", c)
	}

	t.Setenv("SHEETSERVE_CACHE_TTL", "soon")
	if _, err := FromEnv(); err == nil {
		t.Error("bad SHEETSERVE_CACHE_TTL accepted")
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
