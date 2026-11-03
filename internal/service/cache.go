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
//
// Header rows are cached on their own, for at least minHeaderTTL even with caching off, under the
// same rule: the MCP endpoint builds its tools from every reachable tab's header on each request.
type Cache struct {
	ttl time.Duration // 0 turns caching off; locks still apply
	now func() time.Time

	mu         sync.Mutex
	tabs       map[string]*cachedTab
	verified   map[string]time.Time // userKey -> when that user last read the tab from Google
	headers    map[string]cachedHeader
	headerSeen map[string]time.Time // userKey -> when that user last read the header from Google
	locks      sync.Map             // key -> *sync.Mutex
}

// minHeaderTTL is the least time a header row is kept. A tool built from a header up to this old
// is harmless: calling it reads the tab, and a column it lacks or no longer has is refused there.
const minHeaderTTL = time.Minute

type cachedHeader struct {
	row []string
	at  time.Time
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
	return &Cache{ttl: ttl, now: time.Now, tabs: map[string]*cachedTab{}, verified: map[string]time.Time{},
		headers: map[string]cachedHeader{}, headerSeen: map[string]time.Time{}}
}

func (c *Cache) headerTTL() time.Duration { return max(c.ttl, minHeaderTTL) }

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

// put caches t, which user just read from Google, under key, and its header row even with caching
// off, and drops whatever has expired.
func (c *Cache) put(user int64, key string, t *cachedTab) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(t.vals) > 0 {
		c.putHeaderLocked(user, key, headerOf(t.vals), t.at)
	}
	if c.ttl > 0 {
		c.tabs[key] = t
		c.verified[userKey(user, key)] = t.at
	}
	c.sweep()
}

// header returns the header row under key if it was read less than headerTTL ago and user read it
// from Google within headerTTL too.
func (c *Cache) header(user int64, key string) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h, ok := c.headers[key]
	if !ok || c.older(h.at, c.headerTTL()) || c.older(c.headerSeen[userKey(user, key)], c.headerTTL()) {
		return nil, false
	}
	return h.row, true
}

// putHeader caches row, the header under key that user just read from Google.
func (c *Cache) putHeader(user int64, key string, row []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putHeaderLocked(user, key, row, c.now())
	c.sweep()
}

func (c *Cache) putHeaderLocked(user int64, key string, row []string, at time.Time) {
	c.headers[key] = cachedHeader{row, at}
	c.headerSeen[userKey(user, key)] = at
}

// sweep drops expired tabs, headers and reads. The caller holds c.mu.
func (c *Cache) sweep() {
	maps.DeleteFunc(c.tabs, func(_ string, t *cachedTab) bool { return c.expired(t.at) })
	maps.DeleteFunc(c.verified, func(_ string, at time.Time) bool { return c.expired(at) })
	maps.DeleteFunc(c.headers, func(_ string, h cachedHeader) bool { return c.older(h.at, c.headerTTL()) })
	maps.DeleteFunc(c.headerSeen, func(_ string, at time.Time) bool { return c.older(at, c.headerTTL()) })
}

// expired reports whether a tab read at at is too old to use.
func (c *Cache) expired(at time.Time) bool { return c.older(at, c.ttl) }

// older reports whether something read at at is ttl old or more.
func (c *Cache) older(at time.Time, ttl time.Duration) bool { return c.now().Sub(at) >= ttl }

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
