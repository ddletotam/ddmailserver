package server

import "testing"

func TestBodyCacheStoresLargeEntries(t *testing.T) {
	c := newBodyCache()
	big := make([]byte, bodyCacheMinEntry+10)

	c.put(1, big)
	got, ok := c.get(1)
	if !ok {
		t.Fatal("expected large entry to be cached")
	}
	if len(got) != len(big) {
		t.Fatalf("cached length = %d, want %d", len(got), len(big))
	}
}

func TestBodyCacheSkipsSmallEntries(t *testing.T) {
	c := newBodyCache()
	small := make([]byte, bodyCacheMinEntry-1)

	c.put(1, small)
	if _, ok := c.get(1); ok {
		t.Fatal("small entry below threshold should not be cached")
	}
}

func TestBodyCacheDuplicatePutKeepsFirst(t *testing.T) {
	c := newBodyCache()
	first := make([]byte, bodyCacheMinEntry)
	second := make([]byte, bodyCacheMinEntry+100)

	c.put(1, first)
	c.put(1, second) // same id — must be ignored, no double accounting
	if c.bytes != len(first) {
		t.Fatalf("byte accounting = %d, want %d (duplicate put must be ignored)", c.bytes, len(first))
	}
}

func TestBodyCacheNilSafe(t *testing.T) {
	var c *bodyCache
	if _, ok := c.get(1); ok {
		t.Fatal("nil cache get should report miss")
	}
	c.put(1, make([]byte, bodyCacheMinEntry)) // must not panic
}
