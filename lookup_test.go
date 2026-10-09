package geocn

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/lionsoul2014/ip2region/binding/golang/xdb"
	"go.uber.org/zap"
)

func TestGeoCNCacheEmptyResult(t *testing.T) {
	reader, err := openGeoIPFromFile(fixturePath(t, "Country.mmdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	app := &GeoCNApp{dbReader: reader, lock: new(sync.RWMutex), cache: newIPCache(100, time.Minute)}
	const ip = "1.1.1.1" // 本地 CN 数据库不包含此地址。
	if got := app.lookupCountry(ip); got != "" {
		t.Fatalf("expected empty country, got %q", got)
	}
	if got, found := app.cache.Get(ip); !found || got != "" {
		t.Fatalf("successful empty lookup should be cached: value=%q found=%v", got, found)
	}
	app.cache.entries[ip].timestamp = time.Now().Add(-2 * time.Minute)
	if _, found := app.cache.Get(ip); found {
		t.Fatal("empty result should expire")
	}
	app.lookupCountry(ip)
	if _, found := app.cache.Get(ip); !found {
		t.Fatal("expired empty result should be refreshed")
	}

	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	const otherIP = "8.8.8.8"
	app.lookupCountry(otherIP)
	if _, found := app.cache.Get(otherIP); found {
		t.Fatal("database errors must not be cached")
	}
	for _, ip := range []string{"invalid", "127.0.0.1", "10.0.0.1"} {
		app.lookupCountry(ip)
		if _, found := app.cache.Get(ip); found {
			t.Fatalf("invalid/private address %q must not be cached", ip)
		}
	}
}

func TestGeoCityConcurrentLookup(t *testing.T) {
	v4, err := openXDBFromFile(xdb.IPv4, fixturePath(t, "ip2region_v4.xdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer v4.Close()
	v6, err := openXDBFromFile(xdb.IPv6, fixturePath(t, "ip2region_v6.xdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer v6.Close()
	app := &GeoCityApp{searcherIPv4: v4, searcherIPv6: v6, lock: new(sync.RWMutex), logger: zap.NewNop()}
	ips := []string{"114.114.114.114", "8.8.8.8", "::ffff:114.114.114.114", "2400:3200::1"}
	want := make([]string, len(ips))
	for i, ip := range ips {
		want[i] = app.lookupRegion(ip)
		if want[i] == "" {
			t.Fatalf("fixture lookup empty for %s", ip)
		}
	}
	if want[0] != want[2] {
		t.Fatal("IPv4-mapped IPv6 must use the same IPv4 region")
	}
	// 关闭缓存确保每次都进入共享查询器；race 检测可捕获统计字段的并发写入。
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for range 16 {
		wg.Go(func() {
			<-gate
			for range 100 {
				for i, ip := range ips {
					if got := app.lookupRegion(ip); got != want[i] {
						t.Errorf("lookup %s: got %q, want %q", ip, got, want[i])
						return
					}
				}
			}
		})
	}
	close(gate)
	wg.Wait()
}

func TestGeoCityCacheEmptyResult(t *testing.T) {
	// 全零向量索引表示无记录，复现成功但无地区数据的查询。
	searcher, err := xdb.NewWithBuffer(xdb.IPv4, make([]byte, xdb.HeaderInfoLength+xdb.VectorIndexRows*xdb.VectorIndexCols*xdb.VectorIndexSize))
	if err != nil {
		t.Fatal(err)
	}
	app := &GeoCityApp{searcherIPv4: searcher, lock: new(sync.RWMutex), cache: newCityCache(100, time.Minute), logger: zap.NewNop()}
	const ip = "1.1.1.1"
	if got := app.lookupRegion(ip); got != "" {
		t.Fatalf("expected empty region, got %q", got)
	}
	if got, found := app.cache.Get(ip); !found || got != "" {
		t.Fatalf("successful empty lookup should be cached: value=%q found=%v", got, found)
	}
	// 版本不匹配会返回查询错误，不能把错误当作成功空结果缓存。
	const otherIP = "2400:3200::1"
	app.searcherIPv6 = searcher
	app.lookupRegion(otherIP)
	if _, found := app.cache.Get(otherIP); found {
		t.Fatal("database errors must not be cached")
	}
}

func TestGetHost(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"114.114.114.114", "114.114.114.114"},
		{" 114.114.114.114 ", "114.114.114.114"},
		{"114.114.114.114:443", "114.114.114.114"},
		{"2400:3200::1", "2400:3200::1"},
		{"[2400:3200::1]:443", "2400:3200::1"},
		{"::ffff:114.114.114.114", "::ffff:114.114.114.114"},
		{"[fe80::1%en0]:443", "fe80::1%en0"},
		{"example.com:443", "example.com"},
		{"invalid", "invalid"},
		{"", ""},
	} {
		if got := getHost(tc.input); got != tc.want {
			t.Errorf("getHost(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// BenchmarkMatcherCacheHit 覆盖完整 matcher，避免只测缓存而遗漏日志和规则分配。
func BenchmarkMatcherCacheHit(b *testing.B) {
	const ip = "114.114.114.114"
	cnCache := newIPCache(10000, time.Hour)
	cnCache.Set(ip, "CN")
	cityCache := newCityCache(10000, time.Hour)
	cityCache.Set(ip, "中国|江苏省|南京市|电信")
	cn := &GeoCN{app: &GeoCNApp{cache: cnCache}, logger: zap.NewNop()}
	city := &GeoCity{app: &GeoCityApp{cache: cityCache}, logger: zap.NewNop(), allKeywords: [][]string{{"江苏省", "电信"}}}
	request := &http.Request{RemoteAddr: ip + ":443"}
	trusted := request.WithContext(context.WithValue(context.Background(), caddyhttp.VarsCtxKey, map[string]any{caddyhttp.ClientIPVarKey: ip}))
	for _, tc := range []struct {
		name  string
		match func(*http.Request) bool
		req   *http.Request
	}{
		{"GeoCN/Remote", cn.Match, request},
		{"GeoCN/ClientIP", cn.Match, trusted},
		{"GeoCity/Remote", city.Match, request},
		{"GeoCity/ClientIP", city.Match, trusted},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if !tc.match(tc.req) {
					b.Fatal("expected match")
				}
			}
		})
	}
}

func BenchmarkLookup(b *testing.B) {
	cnReader, err := openGeoIPFromFile("Country.mmdb")
	if err != nil {
		b.Fatal(err)
	}
	defer cnReader.Close()
	cn := &GeoCNApp{dbReader: cnReader, lock: new(sync.RWMutex)}
	cnEmpty := &GeoCNApp{dbReader: cnReader, lock: new(sync.RWMutex), cache: newIPCache(10000, time.Hour)}
	cnEmpty.lookupCountry("1.1.1.1")
	v4, err := openXDBFromFile(xdb.IPv4, "ip2region_v4.xdb")
	if err != nil {
		b.Fatal(err)
	}
	defer v4.Close()
	v6, err := openXDBFromFile(xdb.IPv6, "ip2region_v6.xdb")
	if err != nil {
		b.Fatal(err)
	}
	defer v6.Close()
	city := &GeoCityApp{searcherIPv4: v4, searcherIPv6: v6, lock: new(sync.RWMutex), logger: zap.NewNop()}
	for _, tc := range []struct {
		name   string
		lookup func(string) string
		ip     string
	}{
		{"GeoCN/NoCache", cn.lookupCountry, "114.114.114.114"},
		{"GeoCN/EmptyCacheHit", cnEmpty.lookupCountry, "1.1.1.1"},
		{"GeoCity/IPv4", city.lookupRegion, "114.114.114.114"},
		{"GeoCity/IPv6", city.lookupRegion, "2400:3200::1"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				tc.lookup(tc.ip)
			}
		})
	}
	b.Run("GeoCity/IPv4Parallel", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				city.lookupRegion("114.114.114.114")
			}
		})
	})
}
