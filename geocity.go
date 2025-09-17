package geocn

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/lionsoul2014/ip2region/binding/golang/xdb"
	"go.uber.org/zap"
)

var (
	_ caddy.Module                      = (*GeoCity)(nil)
	_ caddyhttp.RequestMatcherWithError = (*GeoCity)(nil)
	_ caddy.Provisioner                 = (*GeoCity)(nil)
	_ caddy.CleanerUpper                = (*GeoCity)(nil)
	_ caddyfile.Unmarshaler             = (*GeoCity)(nil)
)

const (
	ip2regionIPv4RemoteFile = "https://github.com/lionsoul2014/ip2region/raw/master/data/ip2region_v4.xdb"
	ip2regionIPv6RemoteFile = "https://github.com/lionsoul2014/ip2region/raw/master/data/ip2region_v6.xdb"
)

func init() {
	caddy.RegisterModule(GeoCity{})
}

type GeoCity struct {
	// 刷新间隔
	Interval caddy.Duration `json:"interval,omitempty"`
	// 请求超时
	Timeout caddy.Duration `json:"timeout,omitempty"`
	// IPv4 数据库源（可以是 HTTP URL 或本地文件路径）
	IPv4Source string `json:"ipv4_source,omitempty"`
	// IPv6 数据库源（可以是 HTTP URL 或本地文件路径）
	IPv6Source string `json:"ipv6_source,omitempty"`
	// 省份列表（根据mode决定是允许还是拒绝）
	Provinces []string `json:"provinces,omitempty"`
	// 城市列表（根据mode决定是允许还是拒绝）
	Cities []string `json:"cities,omitempty"`
	// 匹配模式：allow（白名单）或 deny（黑名单）
	Mode string `json:"mode,omitempty"`
	// 缓存设置
	EnableCache  *bool          `json:"enable_cache,omitempty"` // 使用指针以区分未设置和false
	CacheTTL     caddy.Duration `json:"cache_ttl,omitempty"`
	CacheMaxSize int            `json:"cache_max_size,omitempty"`

	ctx           caddy.Context
	lock          *sync.RWMutex
	searcherIPv4  *xdb.Searcher // IPv4 查询器
	searcherIPv6  *xdb.Searcher // IPv6 查询器
	localIPv4File string        // IPv4 本地缓存文件路径（自动生成）
	localIPv6File string        // IPv6 本地缓存文件路径（自动生成）
	logger        *zap.Logger
	cache         *cityCache
}

// 城市查询缓存结构
type cityCache struct {
	mu      sync.RWMutex
	entries map[string]*cityCacheEntry
	maxSize int
	ttl     time.Duration
}

type cityCacheEntry struct {
	matched   bool
	timestamp time.Time
}

func (GeoCity) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.geocity",
		New: func() caddy.Module { return new(GeoCity) },
	}
}

// getContext 返回一个可取消的上下文，如果配置了超时则带有超时
func (g *GeoCity) getContext() (context.Context, context.CancelFunc) {
	if g.Timeout > 0 {
		return context.WithTimeout(g.ctx, time.Duration(g.Timeout))
	}
	return context.WithCancel(g.ctx)
}

func (g *GeoCity) getHTTPClient() *http.Client {
	return &http.Client{
		Timeout: time.Duration(g.Timeout),
		Transport: &http.Transport{
			DisableKeepAlives: true,
			IdleConnTimeout:   time.Duration(g.Timeout),
		},
	}
}

func (g *GeoCity) Provision(ctx caddy.Context) error {
	g.ctx = ctx
	g.lock = new(sync.RWMutex)
	g.logger = ctx.Logger(g)

	// 设置默认值
	if g.Mode == "" {
		g.Mode = "allow" // 默认为白名单模式
	}
	if g.Timeout == 0 {
		g.Timeout = caddy.Duration(30 * time.Second)
	}

	// 自动生成本地缓存路径（使用 Caddy 的数据目录）
	caddyDir := caddy.AppDataDir()
	cacheDir := filepath.Join(caddyDir, "geocity")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return fmt.Errorf("failed to create cache directory: %v", err)
	}

	// 根据源地址生成唯一的缓存文件名
	g.localIPv4File = filepath.Join(cacheDir, "ipv4.xdb")
	g.localIPv6File = filepath.Join(cacheDir, "ipv6.xdb")

	// 设置默认数据源
	if g.IPv4Source == "" {
		g.IPv4Source = ip2regionIPv4RemoteFile
	}
	if g.IPv6Source == "" {
		g.IPv6Source = ip2regionIPv6RemoteFile
	}

	// 默认启用缓存
	if g.EnableCache == nil {
		enableCache := true
		g.EnableCache = &enableCache
	}

	// 初始化缓存（如果启用）
	if *g.EnableCache {
		// 非正数容量统一使用默认值，简化语义
		if g.CacheMaxSize <= 0 {
			g.CacheMaxSize = 10000
		}
		if g.CacheTTL == 0 {
			g.CacheTTL = caddy.Duration(5 * time.Minute)
		}
		g.cache = newCityCache(g.CacheMaxSize, time.Duration(g.CacheTTL))
		// 启动缓存清理协程
		go g.cache.cleanup(g.ctx)
		g.logger.Info("City cache enabled",
			zap.Duration("ttl", time.Duration(g.CacheTTL)),
			zap.Int("max_size", g.CacheMaxSize))
	}

	// 加载 IPv4 数据库
	if err := g.loadDatabase(g.IPv4Source, g.localIPv4File, xdb.IPv4, &g.searcherIPv4); err != nil {
		g.logger.Warn("failed to load IPv4 database",
			zap.String("source", g.IPv4Source),
			zap.Error(err))
	}

	// 加载 IPv6 数据库
	if err := g.loadDatabase(g.IPv6Source, g.localIPv6File, xdb.IPv6, &g.searcherIPv6); err != nil {
		g.logger.Warn("failed to load IPv6 database",
			zap.String("source", g.IPv6Source),
			zap.Error(err))
	}

	// 确保至少有一个数据库可用
	if g.searcherIPv4 == nil && g.searcherIPv6 == nil {
		return fmt.Errorf("failed to load any IP database (neither IPv4 nor IPv6)")
	}

	// 启动定期更新
	go g.periodicUpdate()
	return nil
}

// 城市查询缓存实现
func newCityCache(maxSize int, ttl time.Duration) *cityCache {
	return &cityCache{
		entries: make(map[string]*cityCacheEntry),
		maxSize: maxSize,
		ttl:     ttl,
	}
}

// loadDatabase 加载数据库（支持本地文件和 HTTP URL）
func (g *GeoCity) loadDatabase(source, cacheFile string, version *xdb.Version, searcher **xdb.Searcher) error {
	// 先尝试使用缓存文件
	if s, err := xdb.NewWithFileOnly(version, cacheFile); err == nil {
		*searcher = s
		g.logger.Debug("loaded database from cache",
			zap.String("cache", cacheFile),
			zap.String("source", source))
		return nil
	}

	// 缓存文件不存在或无效，从源加载
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		// HTTP 源：下载到缓存
		if err := g.downloadFile(source, cacheFile); err != nil {
			return fmt.Errorf("download from %s: %w", source, err)
		}
	} else {
		// 本地文件源：直接使用
		if _, err := os.Stat(source); err != nil {
			return fmt.Errorf("local file not found: %s", source)
		}
		// 复制到缓存目录（便于统一管理）
		if err := g.copyFile(source, cacheFile); err != nil {
			// 复制失败，直接使用源文件
			cacheFile = source
		}
	}

	// 加载数据库
	s, err := xdb.NewWithFileOnly(version, cacheFile)
	if err != nil {
		return fmt.Errorf("load database: %w", err)
	}
	*searcher = s

	g.logger.Info("loaded database",
		zap.String("source", source),
		zap.String("cache", cacheFile))
	return nil
}

// copyFile 复制文件
func (g *GeoCity) copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)
	return err
}

func (c *cityCache) get(ip string) (bool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, exists := c.entries[ip]
	if !exists {
		return false, false
	}

	// 检查是否过期
	if time.Since(entry.timestamp) > c.ttl {
		return false, false
	}

	return entry.matched, true
}

func (c *cityCache) set(ip string, matched bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 如果缓存已满，清理最老的条目
	if len(c.entries) >= c.maxSize {
		c.evictOldest()
	}

	c.entries[ip] = &cityCacheEntry{
		matched:   matched,
		timestamp: time.Now(),
	}
}

func (c *cityCache) evictOldest() {
	var oldestIP string
	var oldestTime time.Time

	for ip, entry := range c.entries {
		if oldestTime.IsZero() || entry.timestamp.Before(oldestTime) {
			oldestTime = entry.timestamp
			oldestIP = ip
		}
	}

	if oldestIP != "" {
		delete(c.entries, oldestIP)
	}
}

func (c *cityCache) cleanup(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.mu.Lock()
			now := time.Now()
			for ip, entry := range c.entries {
				if now.Sub(entry.timestamp) > c.ttl {
					delete(c.entries, ip)
				}
			}
			c.mu.Unlock()
		case <-ctx.Done():
			return
		}
	}
}

// 检查是否需要更新数据库
func (g *GeoCity) checkNeedUpdate(remoteFile, localFile string) (bool, error) {
	// 如果文件不存在，需要更新
	if _, err := os.Stat(localFile); err != nil {
		return true, nil
	}

	// 通过 Last-Modified 和本地 mtime 对比决定是否更新
	ctx, cancel := g.getContext()
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, remoteFile, nil)
	if err != nil {
		return false, err
	}

	resp, err := g.getHTTPClient().Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("HEAD %s returned %s", remoteFile, resp.Status)
	}

	lm := resp.Header.Get("Last-Modified")
	fi, statErr := os.Stat(localFile)
	if lm == "" {
		// 退回策略：按配置的 Interval 判断是否需要刷新
		if statErr != nil {
			return true, nil
		}
		interval := time.Duration(g.Interval)
		if interval <= 0 {
			interval = 24 * time.Hour
		}
		return time.Since(fi.ModTime()) >= interval, nil
	}
	remoteTime, err := time.Parse(http.TimeFormat, lm)
	if err != nil {
		return false, fmt.Errorf("parse Last-Modified: %w", err)
	}
	if statErr != nil {
		return true, nil
	}
	return remoteTime.After(fi.ModTime()), nil
}

// 更新 IPv4 数据库文件
func (g *GeoCity) updateDatabaseIPv4() error {
	// 如果源不是 HTTP URL，直接加载
	if !strings.HasPrefix(g.IPv4Source, "http://") && !strings.HasPrefix(g.IPv4Source, "https://") {
		return g.loadDatabase(g.IPv4Source, g.localIPv4File, xdb.IPv4, &g.searcherIPv4)
	}

	tempFile := g.localIPv4File + ".temp"

	// 下载到临时文件
	if err := g.downloadFile(g.IPv4Source, tempFile); err != nil {
		os.Remove(tempFile)
		return fmt.Errorf("download IPv4 database failed: %v", err)
	}

	// 验证下载的文件
	tempSearcher, err := xdb.NewWithFileOnly(xdb.IPv4, tempFile)
	if err != nil {
		os.Remove(tempFile)
		return fmt.Errorf("invalid IPv4 database file: %v", err)
	}
	tempSearcher.Close()

	g.lock.Lock()
	defer g.lock.Unlock()

	oldSearcher := g.searcherIPv4

	if err := os.Rename(tempFile, g.localIPv4File); err != nil {
		os.Remove(tempFile)
		return fmt.Errorf("replace IPv4 database file failed: %v", err)
	}

	newSearcher, err := xdb.NewWithFileOnly(xdb.IPv4, g.localIPv4File)
	if err != nil {
		return fmt.Errorf("open new IPv4 database file failed: %v", err)
	}

	g.searcherIPv4 = newSearcher

	if oldSearcher != nil {
		oldSearcher.Close()
	}

	g.logger.Info("IPv4 database updated successfully", zap.String("file", g.localIPv4File))
	return nil
}

// 更新 IPv6 数据库文件
func (g *GeoCity) updateDatabaseIPv6() error {
	// 如果源不是 HTTP URL，直接加载
	if !strings.HasPrefix(g.IPv6Source, "http://") && !strings.HasPrefix(g.IPv6Source, "https://") {
		return g.loadDatabase(g.IPv6Source, g.localIPv6File, xdb.IPv6, &g.searcherIPv6)
	}

	tempFile := g.localIPv6File + ".temp"

	// 下载到临时文件
	if err := g.downloadFile(g.IPv6Source, tempFile); err != nil {
		os.Remove(tempFile)
		return fmt.Errorf("download IPv6 database failed: %v", err)
	}

	// 验证下载的文件
	tempSearcher, err := xdb.NewWithFileOnly(xdb.IPv6, tempFile)
	if err != nil {
		os.Remove(tempFile)
		return fmt.Errorf("invalid IPv6 database file: %v", err)
	}
	tempSearcher.Close()

	g.lock.Lock()
	defer g.lock.Unlock()

	oldSearcher := g.searcherIPv6

	if err := os.Rename(tempFile, g.localIPv6File); err != nil {
		os.Remove(tempFile)
		return fmt.Errorf("replace IPv6 database file failed: %v", err)
	}

	newSearcher, err := xdb.NewWithFileOnly(xdb.IPv6, g.localIPv6File)
	if err != nil {
		return fmt.Errorf("open new IPv6 database file failed: %v", err)
	}

	g.searcherIPv6 = newSearcher

	if oldSearcher != nil {
		oldSearcher.Close()
	}

	g.logger.Info("IPv6 database updated successfully", zap.String("file", g.localIPv6File))
	return nil
}

func (g *GeoCity) downloadFile(remoteURL, localFile string) error {
	ctx, cancel := g.getContext()
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, remoteURL, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	resp, err := g.getHTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("downloading database: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code %d for %s", resp.StatusCode, remoteURL)
	}

	out, err := os.Create(localFile)
	if err != nil {
		return fmt.Errorf("creating file: %w", err)
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return fmt.Errorf("writing file: %w", err)
	}
	return nil
}

func (g *GeoCity) periodicUpdate() {
	if g.Interval == 0 {
		g.Interval = caddy.Duration(time.Hour * 24) // 默认每天更新一次
	}

	ticker := time.NewTicker(time.Duration(g.Interval))
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// 更新 IPv4 数据库
			if g.IPv4Source != "" {
				if strings.HasPrefix(g.IPv4Source, "http://") || strings.HasPrefix(g.IPv4Source, "https://") {
					// 只有 HTTP 源才需要定期更新
					if ok, err := g.checkNeedUpdate(g.IPv4Source, g.localIPv4File); err != nil {
						g.logger.Warn("check IPv4 update failed", zap.Error(err))
					} else if ok {
						if err := g.updateDatabaseIPv4(); err != nil {
							g.logger.Error("update IPv4 database failed", zap.Error(err))
						}
					}
				}
			}

			// 更新 IPv6 数据库
			if g.IPv6Source != "" {
				if strings.HasPrefix(g.IPv6Source, "http://") || strings.HasPrefix(g.IPv6Source, "https://") {
					if ok, err := g.checkNeedUpdate(g.IPv6Source, g.localIPv6File); err != nil {
						g.logger.Warn("check IPv6 update failed", zap.Error(err))
					} else if ok {
						if err := g.updateDatabaseIPv6(); err != nil {
							g.logger.Error("update IPv6 database failed", zap.Error(err))
						}
					}
				}
			}
		case <-g.ctx.Done():
			return
		}
	}
}

func (g *GeoCity) Cleanup() error {
	g.lock.Lock()
	defer g.lock.Unlock()

	if g.searcherIPv4 != nil {
		g.searcherIPv4.Close()
		g.searcherIPv4 = nil
	}
	if g.searcherIPv6 != nil {
		g.searcherIPv6.Close()
		g.searcherIPv6 = nil
	}
	return nil
}

// UnmarshalCaddyfile 实现 caddyfile.Unmarshaler
func (g *GeoCity) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for n := d.Nesting(); d.NextBlock(n); {
			switch d.Val() {
			case "interval":
				if !d.NextArg() {
					return d.ArgErr()
				}
				val, err := caddy.ParseDuration(d.Val())
				if err != nil {
					return err
				}
				g.Interval = caddy.Duration(val)
			case "timeout":
				if !d.NextArg() {
					return d.ArgErr()
				}
				val, err := caddy.ParseDuration(d.Val())
				if err != nil {
					return err
				}
				g.Timeout = caddy.Duration(val)
			case "ipv4_source":
				if !d.NextArg() {
					return d.ArgErr()
				}
				g.IPv4Source = d.Val()
			case "ipv6_source":
				if !d.NextArg() {
					return d.ArgErr()
				}
				g.IPv6Source = d.Val()
			case "mode":
				if !d.NextArg() {
					return d.ArgErr()
				}
				mode := d.Val()
				if mode != "allow" && mode != "deny" {
					return d.Errf("mode must be 'allow' or 'deny', got '%s'", mode)
				}
				g.Mode = mode
			case "provinces":
				args := d.RemainingArgs()
				if len(args) == 0 {
					return d.ArgErr()
				}
				g.Provinces = append(g.Provinces, args...)
			case "cities":
				args := d.RemainingArgs()
				if len(args) == 0 {
					return d.ArgErr()
				}
				g.Cities = append(g.Cities, args...)
			case "cache":
				// 支持 "cache off" 来显式禁用缓存
				if d.NextArg() {
					if d.Val() == "off" {
						enableCache := false
						g.EnableCache = &enableCache
						continue
					}
					// 如果不是 "off"，则回退参数位置
					d.Prev()
				}
				enableCache := true
				g.EnableCache = &enableCache
				for d.NextArg() {
					if d.Val() == "ttl" && d.NextArg() {
						val, err := caddy.ParseDuration(d.Val())
						if err != nil {
							return err
						}
						g.CacheTTL = caddy.Duration(val)
					} else if d.Val() == "size" && d.NextArg() {
						size := d.Val()
						var maxSize int
						if _, err := fmt.Sscanf(size, "%d", &maxSize); err != nil {
							return d.Errf("invalid cache size: %s", size)
						}
						if maxSize <= 0 {
							// 非正数容量：使用默认值逻辑（在 Provision 中处理）
							g.CacheMaxSize = 0
						} else {
							g.CacheMaxSize = maxSize
						}
					}
				}
			default:
				return d.ArgErr()
			}
		}
	}
	return nil
}

// 检查IP是否匹配地理位置规则
func (g *GeoCity) matchLocation(ip net.IP) bool {
	if ip == nil || checkPrivateIP(ip) {
		return false
	}

	// 如果启用缓存，先尝试从缓存获取
	if g.cache != nil {
		if matched, found := g.cache.get(ip.String()); found {
			return matched
		}
	}

	g.lock.RLock()
	defer g.lock.RUnlock()

	// 根据 IP 版本选择对应的查询器
	var searcher *xdb.Searcher
	if ip.To4() != nil {
		// IPv4 地址
		searcher = g.searcherIPv4
		if searcher == nil {
			g.logger.Debug("IPv4 database not available", zap.String("ip", ip.String()))
			return false
		}
	} else {
		// IPv6 地址
		searcher = g.searcherIPv6
		if searcher == nil {
			g.logger.Debug("IPv6 database not available", zap.String("ip", ip.String()))
			return false
		}
	}

	// 查询IP地理位置信息
	region, err := searcher.SearchByStr(ip.String())
	if err != nil {
		g.logger.Debug("failed to search IP location", zap.String("ip", ip.String()), zap.Error(err))
		return false
	}

	// 解析地理位置信息
	// ip2region 返回格式: 国家|区域|省份|城市|ISP
	parts := strings.Split(region, "|")
	if len(parts) < 4 {
		g.logger.Debug("invalid region format", zap.String("region", region))
		return false
	}

	country := strings.TrimSpace(parts[0])
	province := strings.TrimSpace(parts[2])
	city := strings.TrimSpace(parts[3])

	// 如果不是中国IP，根据mode来判断
	var matched bool
	if country != "中国" {
		switch g.Mode {
		case "allow":
			// 白名单模式：非中国IP默认不允许
			matched = false
		case "deny":
			// 黑名单模式：非中国IP默认允许
			matched = true
		default:
			matched = false
		}
	} else {
		g.logger.Debug("IP location info",
			zap.String("ip", ip.String()),
			zap.String("province", province),
			zap.String("city", city))

		switch g.Mode {
		case "allow":
			matched = g.isAllowed(province, city)
		case "deny":
			matched = !g.isDenied(province, city)
		default:
			matched = false
		}
	}

	// 缓存查询结果
	if g.cache != nil {
		g.cache.set(ip.String(), matched)
	}

	return matched
}

// 检查是否在允许列表中
func (g *GeoCity) isAllowed(province, city string) bool {
	// 如果没有配置列表，默认允许所有
	if len(g.Provinces) == 0 && len(g.Cities) == 0 {
		return true
	}

	// 检查省份
	for _, configProvince := range g.Provinces {
		if strings.Contains(province, configProvince) || strings.Contains(configProvince, province) {
			return true
		}
	}

	// 检查城市
	for _, configCity := range g.Cities {
		if strings.Contains(city, configCity) || strings.Contains(configCity, city) {
			return true
		}
	}

	return false
}

// 检查是否在拒绝列表中
func (g *GeoCity) isDenied(province, city string) bool {
	// 检查省份
	for _, configProvince := range g.Provinces {
		if strings.Contains(province, configProvince) || strings.Contains(configProvince, province) {
			return true
		}
	}

	// 检查城市
	for _, configCity := range g.Cities {
		if strings.Contains(city, configCity) || strings.Contains(configCity, city) {
			return true
		}
	}

	return false
}

// MatchWithError 实现 caddyhttp.RequestMatcherWithError
func (g *GeoCity) MatchWithError(r *http.Request) (bool, error) {
	return g.Match(r), nil
}

func (g *GeoCity) Match(r *http.Request) bool {
	// 获取直接连接的 IP
	remoteIP := getIP(r.RemoteAddr)
	if remoteIP != nil && g.matchLocation(remoteIP) {
		return true
	}

	// 检查 X-Forwarded-For
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if clientIP := getIP(strings.Split(xff, ",")[0]); clientIP != nil {
			return g.matchLocation(clientIP)
		}
	}

	// 检查 X-Real-IP
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		if clientIP := getIP(xri); clientIP != nil {
			return g.matchLocation(clientIP)
		}
	}

	return false
}
