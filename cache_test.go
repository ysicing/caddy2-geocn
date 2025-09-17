package geocn

import (
	"fmt"
	"testing"
	"time"
)

func TestIPCache(t *testing.T) {
	cache := newIPCache(100, 5*time.Minute)

	// 测试设置和获取
	cache.set("1.1.1.1", "US")
	country, found := cache.get("1.1.1.1")
	if !found {
		t.Error("expected to find cached entry")
	}
	if country != "US" {
		t.Errorf("expected country US, got %s", country)
	}

	// 测试缓存未命中
	_, found = cache.get("2.2.2.2")
	if found {
		t.Error("expected cache miss for non-existent IP")
	}

	// 测试缓存逐出
	smallCache := newIPCache(2, 5*time.Minute)
	smallCache.set("1.1.1.1", "US")
	smallCache.set("2.2.2.2", "CN")
	smallCache.set("3.3.3.3", "JP") // 应该逐出最老的条目

	_, found = smallCache.get("1.1.1.1")
	if found {
		t.Error("expected oldest entry to be evicted")
	}

	// 测试TTL过期
	expireCache := newIPCache(100, 100*time.Millisecond)
	expireCache.set("1.1.1.1", "US")
	time.Sleep(200 * time.Millisecond)
	_, found = expireCache.get("1.1.1.1")
	if found {
		t.Error("expected entry to be expired")
	}
}

func TestCityCache(t *testing.T) {
	cache := newCityCache(100, 5*time.Minute)

	// 测试设置和获取
	cache.set("1.1.1.1", true)
	matched, found := cache.get("1.1.1.1")
	if !found {
		t.Error("expected to find cached entry")
	}
	if !matched {
		t.Error("expected matched to be true")
	}

	// 测试缓存未命中
	_, found = cache.get("2.2.2.2")
	if found {
		t.Error("expected cache miss for non-existent IP")
	}

	// 测试TTL过期
	expireCache := newCityCache(100, 100*time.Millisecond)
	expireCache.set("1.1.1.1", true)
	time.Sleep(200 * time.Millisecond)
	_, found = expireCache.get("1.1.1.1")
	if found {
		t.Error("expected entry to be expired")
	}
}

func BenchmarkIPCache(b *testing.B) {
	cache := newIPCache(10000, 5*time.Minute)

	// 预热缓存
	for i := 0; i < 1000; i++ {
		ip := fmt.Sprintf("192.168.%d.%d", i/256, i%256)
		cache.set(ip, "CN")
	}

	b.ResetTimer()

	b.Run("Get", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			cache.get("192.168.1.1")
		}
	})

	b.Run("Set", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			ip := fmt.Sprintf("10.0.%d.%d", i/256, i%256)
			cache.set(ip, "US")
		}
	})

	b.Run("Mixed", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if i%2 == 0 {
				cache.get("192.168.1.1")
			} else {
				ip := fmt.Sprintf("172.16.%d.%d", i/256, i%256)
				cache.set(ip, "JP")
			}
		}
	})
}
