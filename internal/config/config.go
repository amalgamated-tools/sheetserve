package config

import (
	"cmp"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/amalgamated-tools/sheetserve/internal/store"
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
	DatabaseURL  string        // SHEETSERVE_DATABASE_URL
	CacheTTL     time.Duration // SHEETSERVE_CACHE_TTL
	CORSOrigins  []string      // SHEETSERVE_CORS_ORIGINS
	AuthRelay    string        // SHEETSERVE_AUTH_RELAY
	RelayOrigins []string      // SHEETSERVE_RELAY_ORIGINS
	Port         int           // PORT
	OpenSignup   bool          // SHEETSERVE_SIGNUP=open: new users can use the server without an admin's approval

	// Who runs this server, named in its privacy policy and terms.
	OperatorName    string // SHEETSERVE_OPERATOR
	OperatorContact string // SHEETSERVE_CONTACT

	// Google Sheets API per-minute quotas the server keeps under; 0 means no limit.
	ProjectQuota store.Quota // SHEETSERVE_GOOGLE_READS_PER_MINUTE, SHEETSERVE_GOOGLE_WRITES_PER_MINUTE
	UserQuota    store.Quota // SHEETSERVE_GOOGLE_USER_READS_PER_MINUTE, SHEETSERVE_GOOGLE_USER_WRITES_PER_MINUTE
	KeyQuota     store.Quota // SHEETSERVE_GOOGLE_KEY_READS_PER_MINUTE, SHEETSERVE_GOOGLE_KEY_WRITES_PER_MINUTE
}

// perMinute reads a per-minute quota from the environment variable name, or def when it's unset.
func perMinute(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s: want a number of calls per minute such as %d, or 0 for no limit, got %q", name, def, v)
	}
	return n, nil
}

// FromEnv reads the server-wide settings: SHEETSERVE_BASE_URL, SHEETSERVE_SECRET, GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET, GOOGLE_PICKER_API_KEY, GOOGLE_PROJECT_NUMBER, PORT, SHEETSERVE_CACHE_TTL, SHEETSERVE_CORS_ORIGINS, SHEETSERVE_AUTH_RELAY, SHEETSERVE_RELAY_ORIGINS, SHEETSERVE_OPERATOR, SHEETSERVE_CONTACT, SHEETSERVE_SIGNUP and the SHEETSERVE_GOOGLE_*_PER_MINUTE quotas.
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

	port := cmp.Or(os.Getenv("PORT"), "8085")
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return nil, fmt.Errorf("PORT: want a port number such as 8085, got %q", port)
	}
	c.Port = p
	c.DatabaseURL = cmp.Or(os.Getenv("SHEETSERVE_DB"), "sheetserve.db")
	switch signup := os.Getenv("SHEETSERVE_SIGNUP"); signup {
	case "", "approval":
	case "open":
		c.OpenSignup = true
	default:
		return nil, fmt.Errorf("SHEETSERVE_SIGNUP: want approval or open, got %q", signup)
	}
	c.OperatorName, c.OperatorContact = os.Getenv("SHEETSERVE_OPERATOR"), os.Getenv("SHEETSERVE_CONTACT")

	if ttl := os.Getenv("SHEETSERVE_CACHE_TTL"); ttl != "" {
		d, err := time.ParseDuration(ttl)
		if err != nil || d < 0 {
			return nil, fmt.Errorf("SHEETSERVE_CACHE_TTL: want a duration such as 60s, got %q", ttl)
		}
		c.CacheTTL = d
	}
	// The Sheets API's default quotas: 300 reads and 300 writes a minute per project, and 60 of each per
	// user. A key gets half its user's, so one busy key can't starve its user's others.
	for _, q := range []struct {
		dst  *int
		name string
		def  int
	}{
		{&c.ProjectQuota.ReadsPerMinute, "SHEETSERVE_GOOGLE_READS_PER_MINUTE", 300},
		{&c.ProjectQuota.WritesPerMinute, "SHEETSERVE_GOOGLE_WRITES_PER_MINUTE", 300},
		{&c.UserQuota.ReadsPerMinute, "SHEETSERVE_GOOGLE_USER_READS_PER_MINUTE", 60},
		{&c.UserQuota.WritesPerMinute, "SHEETSERVE_GOOGLE_USER_WRITES_PER_MINUTE", 60},
		{&c.KeyQuota.ReadsPerMinute, "SHEETSERVE_GOOGLE_KEY_READS_PER_MINUTE", 30},
		{&c.KeyQuota.WritesPerMinute, "SHEETSERVE_GOOGLE_KEY_WRITES_PER_MINUTE", 30},
	} {
		if *q.dst, err = perMinute(q.name, q.def); err != nil {
			return nil, err
		}
	}
	for o := range strings.SplitSeq(os.Getenv("SHEETSERVE_CORS_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.CORSOrigins = append(c.CORSOrigins, o)
		}
	}
	if c.AuthRelay = os.Getenv("SHEETSERVE_AUTH_RELAY"); c.AuthRelay != "" {
		// Google rejects redirect URIs that carry a fragment, so catch one here rather than at sign-in.
		if u, err := url.Parse(c.AuthRelay); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
			strings.Contains(c.AuthRelay, "#") {
			return nil, fmt.Errorf("SHEETSERVE_AUTH_RELAY: want a URL such as https://sheets.example.com/auth/relay, got %q", c.AuthRelay)
		}
	}
	for o := range strings.SplitSeq(os.Getenv("SHEETSERVE_RELAY_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o == "" {
			continue
		}
		scheme, host, _ := strings.Cut(o, "://")
		if _, err := path.Match(host, ""); err != nil || (scheme != "https" && scheme != "http") || host == "" || strings.ContainsAny(host, "/?#@") {
			return nil, fmt.Errorf("SHEETSERVE_RELAY_ORIGINS: want origins such as https://sheetserve-pr-*.onrender.com, got %q", o)
		}
		c.RelayOrigins = append(c.RelayOrigins, o)
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
