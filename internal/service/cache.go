package service

import (
	"maps"
	"strconv"
	"sync"
	"time"
)

// Cache holds tabs as Google returned them, and the write locks for them, by spreadsheet ID and
// tab. One Cache serves every user's Service, so a user's Service can be rebuilt after a dashboard
// change without losing it, and users who added the same spreadsheet share its tabs and locks.
// An entry is raw values; each Service applies its own user's settings to it.
//
// A user reads a cached tab only while they have read it from Google themselves within the TTL, so
// one user refreshing a shared tab doesn't let another who lost access keep reading it.
type Cache struct {
	ttl time.Duration // 0 turns caching off; locks still apply
	now func() time.Time

	mu       sync.Mutex
	tabs     map[string]*cachedTab
	verified map[string]time.Time // userKey -> when that user last read the tab from Google
	locks    sync.Map             // key -> *sync.Mutex
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
	return &Cache{ttl: ttl, now: time.Now, tabs: map[string]*cachedTab{}, verified: map[string]time.Time{}}
}

func cacheKey(spreadsheetID, tab string) string { return spreadsheetID + "/" + tab }

func userKey(user int64, key string) string { return strconv.FormatInt(user, 10) + " " + key }

// get returns the tab under key if it was read less than ttl ago and user read it from Google
// within ttl too.
func (c *Cache) get(user int64, key string) (*cachedTab, bool) {
	if c.ttl <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tabs[key]
	if !ok || t == nil || c.expired(t.at) || c.expired(c.verified[userKey(user, key)]) {
		return nil, false
	}
	return t, true
}

// put caches t, which user just read from Google, under key, and drops whatever has expired.
func (c *Cache) put(user int64, key string, t *cachedTab) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tabs[key] = t
	c.verified[userKey(user, key)] = t.at
	c.sweep()
}

// sweep drops expired tabs and reads. The caller holds c.mu.
func (c *Cache) sweep() {
	maps.DeleteFunc(c.tabs, func(_ string, t *cachedTab) bool { return c.expired(t.at) })
	maps.DeleteFunc(c.verified, func(_ string, at time.Time) bool { return c.expired(at) })
}

// expired reports whether something read at at is too old to use.
func (c *Cache) expired(at time.Time) bool { return c.now().Sub(at) >= c.ttl }

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
