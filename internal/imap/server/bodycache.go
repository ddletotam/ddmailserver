package server

import "sync"

const (
	// bodyCacheMaxBytes caps total memory held by assembled message bodies.
	bodyCacheMaxBytes = 128 * 1024 * 1024 // 128 MB
	// bodyCacheMinEntry is the smallest message we bother caching. Small
	// messages are cheap to reassemble and are fetched in one shot; only large
	// messages get pulled in many BODY[]<from.length> windows, so only those
	// benefit from the cache.
	bodyCacheMinEntry = 256 * 1024 // 256 KB
)

// bodyCache memoizes assembled RFC822 message bytes keyed by message ID.
//
// A delivered message's content (body, HTML, attachments, headers) is
// immutable, so cached entries never need invalidation — flags live elsewhere
// and aren't part of the literal. Management is therefore just size-bounded
// FIFO eviction. Safe for concurrent use.
type bodyCache struct {
	mu      sync.Mutex
	entries map[int64][]byte
	order   []int64
	bytes   int
}

func newBodyCache() *bodyCache {
	return &bodyCache{entries: make(map[int64][]byte)}
}

// get returns the cached bytes for a message, if present.
func (c *bodyCache) get(id int64) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	data, ok := c.entries[id]
	return data, ok
}

// put stores the assembled bytes for a message. No-op for entries below the
// minimum size or above the whole cache ceiling. Evicts oldest entries until
// the total is back under the ceiling.
func (c *bodyCache) put(id int64, data []byte) {
	if c == nil || len(data) < bodyCacheMinEntry || len(data) > bodyCacheMaxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[id]; exists {
		return
	}
	c.entries[id] = data
	c.order = append(c.order, id)
	c.bytes += len(data)
	for c.bytes > bodyCacheMaxBytes && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		if b, ok := c.entries[oldest]; ok {
			c.bytes -= len(b)
			delete(c.entries, oldest)
		}
	}
}
