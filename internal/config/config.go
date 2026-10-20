package config

import (
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

var AllOps = []string{"read", "create", "update", "delete"}

type Config struct {
	Listen       string                 `yaml:"listen"`
	CacheTTL     time.Duration          `yaml:"cache_ttl"`
	CORSOrigins  []string               `yaml:"cors_origins"`
	Spreadsheets map[string]Spreadsheet `yaml:"spreadsheets"`
	Keys         []Key                  `yaml:"keys"`

	byHash map[string]*Key
}

type Spreadsheet struct {
	ID   string             `yaml:"id"`
	Tabs map[string]TabRule `yaml:"tabs"`
}

type TabRule struct {
	Ops    []string `yaml:"ops"`
	Hidden []string `yaml:"hidden"`
}

type Key struct {
	Name string   `yaml:"name"`
	Hash string   `yaml:"hash"`
	Tabs []string `yaml:"tabs"` // "*", "sheet/*" or "sheet/tab"
	Ops  []string `yaml:"ops"`  // optional; narrows the tab's ops
}

// loadConfig reads path; a missing file is an empty config, which runs hosted mode.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		var c Config
		return &c, c.Validate()
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
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
			return fmt.Errorf("key %q: hash must be sha256:<64 hex chars> (see `sheetserve keygen`)", k.Name)
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
