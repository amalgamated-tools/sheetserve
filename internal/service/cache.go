package service

import (
	"maps"
	"sync"
	"time"
)

// Cache holds tabs as Google returned them, and the write locks for them, by spreadsheet ID and
// tab. One Cache serves every user's Service, so a user's Service can be rebuilt after a dashboard
// change without losing it, and users who added the same spreadsheet share its tabs and locks.
// An entry is raw values; each Service applies its own user's settings to it.
type Cache struct {
	ttl time.Duration // 0 turns caching off; locks still apply

	mu    sync.Mutex
	tabs  map[string]*cachedTab
	locks sync.Map // key -> *sync.Mutex
}

// cachedTab is one tab's values from a read, and its date columns read as serial numbers, by
// column, filled in as some user's settings first need them.
type cachedTab struct {
	vals    [][]any
	serials map[int][]any
	at      time.Time
}

// NewCache returns a Cache that keeps a tab for ttl after reading it.
func NewCache(ttl time.Duration) *Cache {
	return &Cache{ttl: ttl, tabs: map[string]*cachedTab{}}
}

func cacheKey(spreadsheetID, tab string) string { return spreadsheetID + "/" + tab }

// get returns the tab under key if it was read less than ttl ago.
func (c *Cache) get(key string) (*cachedTab, bool) {
	if c.ttl <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tabs[key]
	if !ok || t == nil || time.Since(t.at) >= c.ttl {
		return nil, false
	}
	return t, true
}

func (c *Cache) put(key string, t *cachedTab) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	c.tabs[key] = t
	c.mu.Unlock()
}

// drop removes the tab under key and reports whether it was there.
func (c *Cache) drop(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.tabs[key]
	delete(c.tabs, key)
	return ok
}

// serials returns t's serial-number reads of columns cols, and the columns it has none for yet.
func (c *Cache) serials(t *cachedTab, cols []int) (have map[int][]any, missing []int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	have = map[int][]any{}
	for _, col := range cols {
		if v, ok := t.serials[col]; ok {
			have[col] = v
		} else {
			missing = append(missing, col)
		}
	}
	return have, missing
}

// addSerials records serial-number reads of t's columns.
func (c *Cache) addSerials(t *cachedTab, read map[int][]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	maps.Copy(t.serials, read)
}

// lock returns the write lock for the tab under key.
func (c *Cache) lock(key string) *sync.Mutex {
	m, _ := c.locks.LoadOrStore(key, &sync.Mutex{})
	return m.(*sync.Mutex)
}
