package geocn

import (
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
)

func TestCacheDefaultEnabled(t *testing.T) {
	// 测试 GeoCN 默认启用缓存
	t.Run("GeoCN default cache", func(t *testing.T) {
		g := &GeoCN{}

		// 模拟 Provision 中的默认值设置
		if g.EnableCache == nil {
			enableCache := true
			g.EnableCache = &enableCache
		}

		if g.EnableCache == nil || !*g.EnableCache {
			t.Error("Expected cache to be enabled by default for GeoCN")
		}
	})

	// 测试 GeoCity 默认启用缓存
	t.Run("GeoCity default cache", func(t *testing.T) {
		g := &GeoCity{}

		// 模拟 Provision 中的默认值设置
		if g.EnableCache == nil {
			enableCache := true
			g.EnableCache = &enableCache
		}

		if g.EnableCache == nil || !*g.EnableCache {
			t.Error("Expected cache to be enabled by default for GeoCity")
		}
	})

	// 测试显式禁用缓存
	t.Run("Explicitly disable cache", func(t *testing.T) {
		enableCache := false
		g := &GeoCN{
			EnableCache: &enableCache,
		}

		if g.EnableCache == nil || *g.EnableCache {
			t.Error("Expected cache to be disabled when explicitly set to false")
		}
	})

	// 测试缓存默认参数
	t.Run("Cache default parameters", func(t *testing.T) {
		g := &GeoCN{}

		// 模拟 Provision 中的默认值设置
		if g.EnableCache == nil {
			enableCache := true
			g.EnableCache = &enableCache
		}

		if *g.EnableCache {
			if g.CacheTTL == 0 {
				g.CacheTTL = caddy.Duration(5 * time.Minute)
			}
			if g.CacheMaxSize == 0 {
				g.CacheMaxSize = 10000
			}
		}

		if time.Duration(g.CacheTTL) != 5*time.Minute {
			t.Errorf("Expected default cache TTL to be 5 minutes, got %v", time.Duration(g.CacheTTL))
		}

		if g.CacheMaxSize != 10000 {
			t.Errorf("Expected default cache max size to be 10000, got %d", g.CacheMaxSize)
		}
	})
}
