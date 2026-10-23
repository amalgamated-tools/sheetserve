package config

import (
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

var AllOps = []string{"read", "create", "update", "delete"}

// Config is one user's spreadsheets and keys, plus the server-wide settings from FromEnv.
type Config struct {
	Listen       string
	CacheTTL     time.Duration
	CORSOrigins  []string
	Spreadsheets map[string]Spreadsheet
	Keys         []Key

	byHash map[string]*Key
}

type Spreadsheet struct {
	ID             string
	ServiceAccount bool // reached with the server's service account instead of the owner's Google token
	Tabs           map[string]TabRule
}

type TabRule struct {
	Ops    []string
	Hidden []string
	Key    string // the column that identifies a row; "" means "id"
}

// KeyColumn returns the column that identifies a row. A tab whose header lacks it can only be listed.
func (r TabRule) KeyColumn() string { return cmp.Or(r.Key, "id") }

type Key struct {
	Name string
	Hash string
	Tabs []string // "*", "sheet/*" or "sheet/tab"
	Ops  []string // optional; narrows the tab's ops
}

// FromEnv reads the server-wide settings: PORT, SHEETSERVE_CACHE_TTL and SHEETSERVE_CORS_ORIGINS.
func FromEnv() (*Config, error) {
	var c Config
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
	return &c, c.Validate()
}

func (c *Config) Validate() error {
	c.Listen = cmp.Or(c.Listen, ":"+cmp.Or(os.Getenv("PORT"), "8080"))
	for name, ss := range c.Spreadsheets {
		if ss.ID == "" {
			return fmt.Errorf("spreadsheet %q: missing id", name)
		}
		if strings.Contains(name, "/") {
			return fmt.Errorf("spreadsheet %q: name must not contain /", name)
		}
		for tab, rule := range ss.Tabs {
			if err := checkOps(rule.Ops); err != nil {
				return fmt.Errorf("tab %s/%s: %w", name, tab, err)
			}
		}
	}
	c.byHash = map[string]*Key{}
	for i := range c.Keys {
		k := &c.Keys[i]
		if k.Name == "" {
			return fmt.Errorf("key #%d: missing name", i+1)
		}
		if h, ok := strings.CutPrefix(k.Hash, "sha256:"); !ok || len(h) != 64 {
			return fmt.Errorf("key %q: hash must be sha256:<64 hex chars>", k.Name)
		}
		if _, dup := c.byHash[k.Hash]; dup {
			return fmt.Errorf("key %q: duplicate hash", k.Name)
		}
		if err := checkOps(k.Ops); err != nil {
			return fmt.Errorf("key %q: %w", k.Name, err)
		}
		for _, t := range k.Tabs {
			sheet, tab, _ := strings.Cut(t, "/")
			if t == "*" {
				continue
			}
			ss, ok := c.Spreadsheets[sheet]
			if _, tabOK := ss.Tabs[tab]; !ok || (tab != "*" && !tabOK) {
				return fmt.Errorf("key %q: unknown tab %q", k.Name, t)
			}
		}
		c.byHash[k.Hash] = k
	}
	return nil
}

func checkOps(ops []string) error {
	for _, op := range ops {
		if !slices.Contains(AllOps, op) {
			return fmt.Errorf("unknown op %q (want one of %v)", op, AllOps)
		}
	}
	return nil
}

// newKey returns a fresh API key; only its hashKey is stored.
func NewKey() string { return "sk_" + rand.Text() }

func HashKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// keyFor returns the key matching token, or nil.
func (c *Config) KeyFor(token string) *Key {
	if token == "" {
		return nil
	}
	return c.byHash[HashKey(token)]
}

func (c *Config) Rule(sheet, tab string) (TabRule, bool) {
	r, ok := c.Spreadsheets[sheet].Tabs[tab]
	return r, ok
}

// can reports whether k may perform op on sheet/tab, combining the tab's ops with the key's scope.
func (c *Config) Can(k *Key, sheet, tab, op string) bool {
	rule, ok := c.Rule(sheet, tab)
	if !ok || !slices.Contains(rule.Ops, op) || (len(k.Ops) > 0 && !slices.Contains(k.Ops, op)) {
		return false
	}
	return slices.ContainsFunc(k.Tabs, func(t string) bool {
		return t == "*" || t == sheet+"/*" || t == sheet+"/"+tab
	})
}
