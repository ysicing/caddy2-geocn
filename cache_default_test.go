package geocn

import (
	"testing"
	"time"
)

func TestIPCacheBehavior(t *testing.T) {
	t.Run("cache hit and miss", func(t *testing.T) {
		cache := newIPCache(100, 5*time.Minute)

		// Cache miss
		_, found := cache.Get("192.168.1.1")
		if found {
			t.Error("Expected cache miss for new key")
		}

		// Set value
		cache.Set("192.168.1.1", "CN")

		// Cache hit
		country, found := cache.Get("192.168.1.1")
		if !found {
			t.Error("Expected cache hit after Set")
		}
		if country != "CN" {
			t.Errorf("Expected country CN, got %s", country)
		}
	})

	t.Run("cache TTL expiration", func(t *testing.T) {
		cache := newIPCache(100, 50*time.Millisecond)

		cache.Set("10.0.0.1", "US")

		// Should hit immediately
		_, found := cache.Get("10.0.0.1")
		if !found {
			t.Error("Expected cache hit before TTL expiration")
		}

		// Wait for TTL to expire
		time.Sleep(60 * time.Millisecond)

		// Should miss after TTL
		_, found = cache.Get("10.0.0.1")
		if found {
			t.Error("Expected cache miss after TTL expiration")
		}
	})

	t.Run("cache max size eviction", func(t *testing.T) {
		cache := newIPCache(3, 5*time.Minute)

		cache.Set("1.1.1.1", "A")
		cache.Set("2.2.2.2", "B")
		cache.Set("3.3.3.3", "C")

		// All should be present
		if _, found := cache.Get("1.1.1.1"); !found {
			t.Error("Expected 1.1.1.1 to be in cache")
		}

		// Add one more, should evict oldest
		cache.Set("4.4.4.4", "D")

		// Newest should be present
		if _, found := cache.Get("4.4.4.4"); !found {
			t.Error("Expected 4.4.4.4 to be in cache")
		}
	})
}
