package geocn

import (
	"fmt"
	"testing"
	"time"
)

func TestIPCache(t *testing.T) {
	cache := newIPCache(100, 5*time.Minute)

	// Test set and get
	cache.Set("1.1.1.1", "US")
	country, found := cache.Get("1.1.1.1")
	if !found {
		t.Error("expected to find cached entry")
	}
	if country != "US" {
		t.Errorf("expected country US, got %s", country)
	}

	// Test cache miss
	_, found = cache.Get("2.2.2.2")
	if found {
		t.Error("expected cache miss for non-existent IP")
	}

	// Test eviction
	smallCache := newIPCache(2, 5*time.Minute)
	smallCache.Set("1.1.1.1", "US")
	smallCache.Set("2.2.2.2", "CN")
	smallCache.Set("3.3.3.3", "JP") // should evict oldest entry

	_, found = smallCache.Get("1.1.1.1")
	if found {
		t.Error("expected oldest entry to be evicted")
	}

	// Test TTL expiration
	expireCache := newIPCache(100, 100*time.Millisecond)
	expireCache.Set("1.1.1.1", "US")
	time.Sleep(200 * time.Millisecond)
	_, found = expireCache.Get("1.1.1.1")
	if found {
		t.Error("expected entry to be expired")
	}
}

func TestCityCache(t *testing.T) {
	cache := newCityCache(100, 5*time.Minute)

	// Test set and get
	cache.Set("1.1.1.1", true)
	matched, found := cache.Get("1.1.1.1")
	if !found {
		t.Error("expected to find cached entry")
	}
	if !matched {
		t.Error("expected matched to be true")
	}

	// Test cache miss
	_, found = cache.Get("2.2.2.2")
	if found {
		t.Error("expected cache miss for non-existent IP")
	}

	// Test TTL expiration
	expireCache := newCityCache(100, 100*time.Millisecond)
	expireCache.Set("1.1.1.1", true)
	time.Sleep(200 * time.Millisecond)
	_, found = expireCache.Get("1.1.1.1")
	if found {
		t.Error("expected entry to be expired")
	}
}

func BenchmarkIPCache(b *testing.B) {
	cache := newIPCache(10000, 5*time.Minute)

	// Warm up cache
	for i := 0; i < 1000; i++ {
		ip := fmt.Sprintf("192.168.%d.%d", i/256, i%256)
		cache.Set(ip, "CN")
	}

	b.ResetTimer()

	b.Run("Get", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			cache.Get("192.168.1.1")
		}
	})

	b.Run("Set", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			ip := fmt.Sprintf("10.0.%d.%d", i/256, i%256)
			cache.Set(ip, "US")
		}
	})

	b.Run("Mixed", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if i%2 == 0 {
				cache.Get("192.168.1.1")
			} else {
				ip := fmt.Sprintf("172.16.%d.%d", i/256, i%256)
				cache.Set(ip, "JP")
			}
		}
	})
}
