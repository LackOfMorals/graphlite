package graphlite

import (
	"container/list"
	"sync"

	glsql "github.com/LackOfMorals/graphlite/v2/sql"
)

// planCacheMaxSize is the maximum number of entries kept in the plan cache.
// Once the cache reaches this size, the least-recently-used entry is evicted
// before a new one is inserted.
const planCacheMaxSize = 1024

// planCacheEntry is the value stored in the cache map and linked list.
type planCacheEntry struct {
	key    string      // the Cypher query string (cache key)
	result glsql.Result // the pre-BindParams translation result (read-only after insertion)
}

// planCache is a bounded LRU cache that maps Cypher query strings to their
// pre-BindParams translation results (the output of parse→plan→translate).
//
// Only the unbound result (containing paramSentinel values in Args) is cached.
// BindParams is still called on every invocation to substitute concrete values.
//
// planCache is safe for concurrent use from multiple goroutines.
type planCache struct {
	mu      sync.Mutex
	cap     int
	items   map[string]*list.Element // key → *list.Element (value: *planCacheEntry)
	order   *list.List               // front = most recently used
}

// newPlanCache creates a new planCache with the given capacity.
// A capacity <= 0 is treated as planCacheMaxSize.
func newPlanCache(size int) *planCache {
	if size <= 0 {
		size = planCacheMaxSize
	}
	return &planCache{
		cap:   size,
		items: make(map[string]*list.Element, size),
		order: list.New(),
	}
}

// get looks up a cached result by key. Returns (result, true) on a cache hit
// and (zero, false) on a miss. On a hit, the entry is moved to the front (most
// recently used) of the eviction list.
func (c *planCache) get(key string) (glsql.Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return glsql.Result{}, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*planCacheEntry).result, true
}

// put inserts or updates a key→result mapping. If the cache is full, the
// least-recently-used entry is evicted before the new entry is inserted.
func (c *planCache) put(key string, result glsql.Result) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Update existing entry.
	if el, ok := c.items[key]; ok {
		el.Value.(*planCacheEntry).result = result
		c.order.MoveToFront(el)
		return
	}

	// Evict LRU entry when at capacity.
	if c.order.Len() >= c.cap {
		tail := c.order.Back()
		if tail != nil {
			c.order.Remove(tail)
			delete(c.items, tail.Value.(*planCacheEntry).key)
		}
	}

	// Insert new entry at the front.
	entry := &planCacheEntry{key: key, result: result}
	el := c.order.PushFront(entry)
	c.items[key] = el
}
