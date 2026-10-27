package config

import (
	"cmp"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

var (
	ErrMissingBaseURL             = errors.New("missing required configuration field: SHEETSERVE_BASE_URL")
	ErrMissingSecret              = errors.New("missing required configuration field: SHEETSERVE_SECRET")
	ErrInvalidSecret              = errors.New("SHEETSERVE_SECRET must be 32 random bytes, base64 (openssl rand -base64 32)")
	ErrMissingGoogleClientID      = errors.New("missing required configuration field: GOOGLE_CLIENT_ID")
	ErrInvalidGoogleClientID      = errors.New("invalid GOOGLE_CLIENT_ID")
	ErrMissingGoogleClientSecret  = errors.New("missing required configuration field: GOOGLE_CLIENT_SECRET")
	ErrMissingGooglePickerAPIKey  = errors.New("missing required configuration field: GOOGLE_PICKER_API_KEY")
	ErrMissingGoogleProjectNumber = errors.New("missing required configuration field: GOOGLE_PROJECT_NUMBER")
	ErrInvalidGoogleProjectNumber = errors.New("GOOGLE_PROJECT_NUMBER must be the numeric project number (the digits before the dash in GOOGLE_CLIENT_ID), not the project ID")
)

// Config is the server-wide settings. Each user's spreadsheets and keys are db.Settings.
type Config struct {
	// Required server-wide settings
	BaseURL             string // SHEETSERVE_BASE_URL
	Secret              string // SHEETSERVE_SECRET
	DecryptedSecret     []byte // decoded SHEETSERVE_SECRET
	GoogleClientID      string // GOOGLE_CLIENT_ID
	GoogleClientSecret  string // GOOGLE_CLIENT_SECRET
	GooglePickerAPIKey  string // GOOGLE_PICKER_API_KEY
	GoogleProjectNumber string // GOOGLE_PROJECT_NUMBER

	// Optional server-wide settings
	DatabaseURL string        // SHEETSERVE_DATABASE_URL
	CacheTTL    time.Duration // SHEETSERVE_CACHE_TTL
	CORSOrigins []string      // SHEETSERVE_CORS_ORIGINS
	Listen      string        // PORT
}

// FromEnv reads the server-wide settings: SHEETSERVE_BASE_URL, SHEETSERVE_SECRET, GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET, GOOGLE_PICKER_API_KEY, GOOGLE_PROJECT_NUMBER, PORT, SHEETSERVE_CACHE_TTL and SHEETSERVE_CORS_ORIGINS.
func FromEnv() (*Config, error) {
	c := Config{
		BaseURL:             cmp.Or(os.Getenv("SHEETSERVE_BASE_URL"), os.Getenv("RENDER_EXTERNAL_URL"), "http://localhost:8085"),
		Secret:              cmp.Or(os.Getenv("SHEETSERVE_SECRET"), ""),
		GoogleClientID:      cmp.Or(os.Getenv("GOOGLE_CLIENT_ID"), ""),
		GoogleClientSecret:  cmp.Or(os.Getenv("GOOGLE_CLIENT_SECRET"), ""),
		GooglePickerAPIKey:  cmp.Or(os.Getenv("GOOGLE_PICKER_API_KEY"), ""),
		GoogleProjectNumber: cmp.Or(os.Getenv("GOOGLE_PROJECT_NUMBER"), ""),
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	c.Listen = cmp.Or(os.Getenv("PORT"), "8085")
	c.DatabaseURL = cmp.Or(os.Getenv("SHEETSERVE_DB"), "sheetserve.db")

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

// Validate checks if the required configuration fields are set.
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("config is nil")
	}
	if c.BaseURL == "" {
		return ErrMissingBaseURL
	}
	if c.Secret == "" {
		return ErrMissingSecret
	}
	secret, err := base64.StdEncoding.DecodeString(c.Secret)
	if err != nil || len(secret) != 32 {
		return ErrInvalidSecret
	}
	c.DecryptedSecret = secret
	if c.GoogleClientID == "" {
		return ErrMissingGoogleClientID
	}
	// validate that the client id matches the expected format
	if !strings.HasSuffix(c.GoogleClientID, ".apps.googleusercontent.com") {
		return ErrInvalidGoogleClientID
	}
	if c.GoogleClientSecret == "" {
		return ErrMissingGoogleClientSecret
	}
	if c.GooglePickerAPIKey == "" {
		return ErrMissingGooglePickerAPIKey
	}
	if c.GoogleProjectNumber == "" {
		return ErrMissingGoogleProjectNumber
	}
	if strings.Trim(c.GoogleProjectNumber, "0123456789") != "" {
		return ErrInvalidGoogleProjectNumber
	}
	return nil
}
