package db

import (
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

var AllOps = []string{"read", "create", "update", "delete"}

// Settings is one user's spreadsheets and keys, as UserSettings reads them.
type Settings struct {
	Spreadsheets map[string]SheetRule
	Keys         []Key

	byHash map[string]*Key
}

type SheetRule struct {
	ID   string
	Tabs map[string]TabRule
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

// ValidationError is a change the settings reject; its message is meant for the user.
type ValidationError string

func (e ValidationError) Error() string { return string(e) }

func (s *Settings) Validate() error {
	for name, ss := range s.Spreadsheets {
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
	s.byHash = map[string]*Key{}
	for i := range s.Keys {
		k := &s.Keys[i]
		if k.Name == "" {
			return fmt.Errorf("key #%d: missing name", i+1)
		}
		if h, ok := strings.CutPrefix(k.Hash, "sha256:"); !ok || len(h) != 64 {
			return fmt.Errorf("key %q: hash must be sha256:<64 hex chars>", k.Name)
		}
		if _, dup := s.byHash[k.Hash]; dup {
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
			ss, ok := s.Spreadsheets[sheet]
			if _, tabOK := ss.Tabs[tab]; !ok || (tab != "*" && !tabOK) {
				return fmt.Errorf("key %q: unknown tab %q", k.Name, t)
			}
		}
		s.byHash[k.Hash] = k
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

// NewKey returns a fresh API key; only its HashKey is stored.
func NewKey() string { return "sk_" + rand.Text() }

func HashKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// KeyFor returns the key matching token, or nil.
func (s *Settings) KeyFor(token string) *Key {
	if token == "" {
		return nil
	}
	return s.byHash[HashKey(token)]
}

func (s *Settings) Rule(sheet, tab string) (TabRule, bool) {
	r, ok := s.Spreadsheets[sheet].Tabs[tab]
	return r, ok
}

// Can reports whether k may perform op on sheet/tab, combining the tab's ops with the key's scope.
func (s *Settings) Can(k *Key, sheet, tab, op string) bool {
	rule, ok := s.Rule(sheet, tab)
	if !ok || !slices.Contains(rule.Ops, op) || (len(k.Ops) > 0 && !slices.Contains(k.Ops, op)) {
		return false
	}
	return slices.ContainsFunc(k.Tabs, func(t string) bool {
		return t == "*" || t == sheet+"/*" || t == sheet+"/"+tab
	})
}
