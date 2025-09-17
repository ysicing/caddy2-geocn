package geocn

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/oschwald/geoip2-golang/v2"
	"go.uber.org/zap"
)

var (
	_ caddy.Module                      = (*GeoCN)(nil)
	_ caddyhttp.RequestMatcherWithError = (*GeoCN)(nil)
	_ caddy.Provisioner                 = (*GeoCN)(nil)
	_ caddy.CleanerUpper                = (*GeoCN)(nil)
	_ caddyfile.Unmarshaler             = (*GeoCN)(nil)
)

const (
	remotefile = "https://github.com/Hackl0us/GeoIP2-CN/raw/release/Country.mmdb"
)

func init() {
	caddy.RegisterModule(GeoCN{})
}

type GeoCN struct {
	// refresh Interval
	Interval caddy.Duration `json:"interval,omitempty"`
	// request Timeout
	Timeout caddy.Duration `json:"timeout,omitempty"`
	// 数据库源（可以是 HTTP URL 或本地文件路径）
	Source string `json:"source,omitempty"`
	// Cache settings
	EnableCache  *bool          `json:"enable_cache,omitempty"` // 使用指针以区分未设置和false
	CacheTTL     caddy.Duration `json:"cache_ttl,omitempty"`
	CacheMaxSize int            `json:"cache_max_size,omitempty"`

	ctx       caddy.Context
	lock      *sync.RWMutex
	dbReader  *geoip2.Reader
	logger    *zap.Logger
	cache     *ipCache
	localFile string // 本地缓存文件路径（自动生成）
}

// IP查询缓存结构
type ipCache struct {
	mu      sync.RWMutex
	entries map[string]*cacheEntry
	maxSize int
	ttl     time.Duration
}

type cacheEntry struct {
	country   string
	timestamp time.Time
}

func (GeoCN) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.geocn",
		New: func() caddy.Module { return new(GeoCN) },
	}
}

// getContext returns a cancelable context, with a timeout if configured.
func (s *GeoCN) getContext() (context.Context, context.CancelFunc) {
	if s.Timeout > 0 {
		return context.WithTimeout(s.ctx, time.Duration(s.Timeout))
	}
	return context.WithCancel(s.ctx)
}

func (m *GeoCN) getHTTPClient() *http.Client {
	return &http.Client{
		Timeout: time.Duration(m.Timeout),
		Transport: &http.Transport{
			DisableKeepAlives: true,
			IdleConnTimeout:   time.Duration(m.Timeout),
		},
	}
}

func (m *GeoCN) validSource(host string) bool {
	if host == "" {
		return false
	}

	// 私网/无效地址直接拒绝
	if tip := net.ParseIP(strings.TrimSpace(host)); tip == nil || checkPrivateIP(tip) {
		return false
	}

	// 如果启用缓存，先尝试从缓存获取（按原始字符串作为 key）
	if m.cache != nil {
		if country, found := m.cache.get(host); found {
			return country == "CN"
		}
	}

	// 解析为 netip.Addr 用于 v2 Reader 查询
	nip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}

	m.lock.RLock()
	defer m.lock.RUnlock()

	// 检查 dbReader 是否初始化
	if m.dbReader == nil {
		return false
	}

	record, err := m.dbReader.Country(nip)
	if err != nil || record == nil || !record.HasData() {
		return false
	}

	country := record.Country.ISOCode

	// 缓存查询结果
	if m.cache != nil && country != "" {
		m.cache.set(host, country)
	}

	return country == "CN"
}

func (m *GeoCN) Provision(ctx caddy.Context) error {
	m.ctx = ctx
	m.lock = new(sync.RWMutex)
	m.logger = ctx.Logger(m)

	// 设置默认值
	if m.Source == "" {
		m.Source = remotefile
	}
	// 自动生成本地缓存路径（使用 Caddy 的数据目录）
	caddyDir := caddy.AppDataDir()
	cacheDir := filepath.Join(caddyDir, "geocn")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return fmt.Errorf("failed to create cache directory: %v", err)
	}
	m.localFile = filepath.Join(cacheDir, "Country.mmdb")

	// 设置默认超时
	if m.Timeout == 0 {
		m.Timeout = caddy.Duration(30 * time.Second)
	}

	// 默认启用缓存
	if m.EnableCache == nil {
		enableCache := true
		m.EnableCache = &enableCache
	}

	// 初始化缓存（如果启用）
	if *m.EnableCache {
		// 非正数容量统一使用默认值，简化语义
		if m.CacheMaxSize <= 0 {
			m.CacheMaxSize = 10000
		}
		if m.CacheTTL == 0 {
			m.CacheTTL = caddy.Duration(5 * time.Minute)
		}
		m.cache = newIPCache(m.CacheMaxSize, time.Duration(m.CacheTTL))
		// 启动缓存清理协程
		go m.cache.cleanup(m.ctx)
		m.logger.Info("IP cache enabled",
			zap.Duration("ttl", time.Duration(m.CacheTTL)),
			zap.Int("max_size", m.CacheMaxSize))
	}

	// 加载数据库
	if err := m.loadDatabase(); err != nil {
		return fmt.Errorf("failed to load GeoIP database: %v", err)
	}

	go m.periodicUpdate()
	return nil
}

// loadDatabase 加载数据库（支持本地文件和 HTTP URL）
func (m *GeoCN) loadDatabase() error {
	// 先尝试使用缓存文件
	if reader, err := geoip2.Open(m.localFile); err == nil {
		m.lock.Lock()
		m.dbReader = reader
		m.lock.Unlock()
		m.logger.Debug("loaded database from cache",
			zap.String("cache", m.localFile),
			zap.String("source", m.Source))
		return nil
	}

	// 缓存文件不存在或无效，从源加载
	if strings.HasPrefix(m.Source, "http://") || strings.HasPrefix(m.Source, "https://") {
		// HTTP 源：下载到缓存
		if err := m.downloadFile(m.localFile); err != nil {
			return fmt.Errorf("download from %s: %w", m.Source, err)
		}
	} else {
		// 本地文件源：直接使用或复制到缓存
		if _, err := os.Stat(m.Source); err != nil {
			return fmt.Errorf("local file not found: %s", m.Source)
		}
		// 复制到缓存目录
		if err := m.copyFile(m.Source, m.localFile); err != nil {
			// 复制失败，直接使用源文件
			m.localFile = m.Source
		}
	}

	// 加载数据库
	reader, err := geoip2.Open(m.localFile)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}

	m.lock.Lock()
	m.dbReader = reader
	m.lock.Unlock()

	m.logger.Info("loaded database",
		zap.String("source", m.Source),
		zap.String("cache", m.localFile))
	return nil
}

// copyFile 复制文件
func (m *GeoCN) copyFile(src, dst string) error {
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

// IP缓存实现
func newIPCache(maxSize int, ttl time.Duration) *ipCache {
	return &ipCache{
		entries: make(map[string]*cacheEntry),
		maxSize: maxSize,
		ttl:     ttl,
	}
}

func (c *ipCache) get(ip string) (string, bool) {
	c.mu.RLock()
	entry, exists := c.entries[ip]
	if !exists {
		c.mu.RUnlock()
		return "", false
	}

	// 检查是否过期
	if time.Since(entry.timestamp) > c.ttl {
		c.mu.RUnlock()
		return "", false
	}

	c.mu.RUnlock()
	return entry.country, true
}

func (c *ipCache) set(ip, country string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 如果缓存已满，清理最老的条目
	if len(c.entries) >= c.maxSize {
		c.evictOldest()
	}

	c.entries[ip] = &cacheEntry{
		country:   country,
		timestamp: time.Now(),
	}
}

func (c *ipCache) evictOldest() {
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

func (c *ipCache) cleanup(ctx context.Context) {
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

// 检查是否需要更新
func (m *GeoCN) checkNeedUpdate() (bool, error) {
	// 如果不是 HTTP 源，不需要更新
	if !strings.HasPrefix(m.Source, "http://") && !strings.HasPrefix(m.Source, "https://") {
		return false, nil
	}

	// 如果文件不存在，需要更新
	if _, err := os.Stat(m.localFile); err != nil {
		return true, nil
	}

	// 通过 Last-Modified 和本地 mtime 对比决定是否更新
	ctx, cancel := m.getContext()
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, m.Source, nil)
	if err != nil {
		return false, err
	}

	resp, err := m.getHTTPClient().Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("HEAD %s returned %s", m.Source, resp.Status)
	}

	lm := resp.Header.Get("Last-Modified")
	fi, statErr := os.Stat(m.localFile)
	if lm == "" {
		// 退回策略：按配置的 Interval 判断是否需要刷新
		if statErr != nil {
			return true, nil
		}
		interval := time.Duration(m.Interval)
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

// 更新文件
func (m *GeoCN) updateGeoFile() error {
	// 如果不是 HTTP 源，直接加载
	if !strings.HasPrefix(m.Source, "http://") && !strings.HasPrefix(m.Source, "https://") {
		return m.loadDatabase()
	}

	tempFile := m.localFile + ".temp"
	if err := m.downloadFile(tempFile); err != nil {
		os.Remove(tempFile)
		return fmt.Errorf("download failed: %v", err)
	}

	// 验证下载的文件
	tempReader, err := geoip2.Open(tempFile)
	if err != nil {
		os.Remove(tempFile)
		return fmt.Errorf("invalid database file: %v", err)
	}
	tempReader.Close()

	m.lock.Lock()
	defer m.lock.Unlock()

	oldReader := m.dbReader

	if err := os.Rename(tempFile, m.localFile); err != nil {
		os.Remove(tempFile)
		return fmt.Errorf("replace database file failed: %v", err)
	}

	newReader, err := geoip2.Open(m.localFile)
	if err != nil {
		return fmt.Errorf("open new database file failed: %v", err)
	}

	m.dbReader = newReader

	if oldReader != nil {
		oldReader.Close()
	}

	m.logger.Info("GeoIP database updated successfully", zap.String("file", m.localFile))
	return nil
}

func (m *GeoCN) downloadFile(file string) error {
	ctx, cancel := m.getContext()
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.Source, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	resp, err := m.getHTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("downloading database: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code %d for %s", resp.StatusCode, m.Source)
	}

	out, err := os.Create(file)
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

func (m *GeoCN) periodicUpdate() {
	if m.Interval == 0 {
		m.Interval = caddy.Duration(24 * time.Hour)
	}

	ticker := time.NewTicker(time.Duration(m.Interval))
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if ok, err := m.checkNeedUpdate(); err != nil {
				m.logger.Warn("check update failed", zap.Error(err))
			} else if ok {
				if err := m.updateGeoFile(); err != nil {
					m.logger.Error("update database failed", zap.Error(err))
				}
			}
		case <-m.ctx.Done():
			return
		}
	}
}

func (m *GeoCN) Cleanup() error {
	m.lock.Lock()
	defer m.lock.Unlock()

	if m.dbReader != nil {
		err := m.dbReader.Close()
		m.dbReader = nil
		return err
	}
	return nil
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (m *GeoCN) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
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
				m.Interval = caddy.Duration(val)
			case "timeout":
				if !d.NextArg() {
					return d.ArgErr()
				}
				val, err := caddy.ParseDuration(d.Val())
				if err != nil {
					return err
				}
				m.Timeout = caddy.Duration(val)
			case "source":
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.Source = d.Val()
			case "cache":
				// 支持 "cache off" 来显式禁用缓存
				if d.NextArg() {
					if d.Val() == "off" {
						enableCache := false
						m.EnableCache = &enableCache
						continue
					}
					// 如果不是 "off"，则回退参数位置
					d.Prev()
				}
				enableCache := true
				m.EnableCache = &enableCache
				for d.NextArg() {
					if d.Val() == "ttl" && d.NextArg() {
						val, err := caddy.ParseDuration(d.Val())
						if err != nil {
							return err
						}
						m.CacheTTL = caddy.Duration(val)
					} else if d.Val() == "size" && d.NextArg() {
						size := d.Val()
						var maxSize int
						if _, err := fmt.Sscanf(size, "%d", &maxSize); err != nil {
							return d.Errf("invalid cache size: %s", size)
						}
						if maxSize <= 0 {
							// 非正数容量：使用默认值逻辑（在 Provision 中处理）
							m.CacheMaxSize = 0
						} else {
							m.CacheMaxSize = maxSize
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

func checkPrivateIP(ip net.IP) bool {
	// 127.0.0.0/8
	// 224.0.0.0/4
	// 169.254.0.0/16
	// 10.0.0.0/8
	// 172.16.0.0/12
	// 192.168.0.0/16
	if ip.IsLoopback() || ip.IsLinkLocalMulticast() || ip.IsLinkLocalUnicast() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	return false
}

// MatchWithError implements caddyhttp.RequestMatcherWithError
func (m *GeoCN) MatchWithError(r *http.Request) (bool, error) {
	return m.Match(r), nil
}

func (m *GeoCN) Match(r *http.Request) bool {
	// 获取直接连接的 IP
	host := getHost(r.RemoteAddr)
	if host != "" && m.validSource(host) {
		return true
	}

	// 检查 X-Forwarded-For
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		client := strings.TrimSpace(strings.Split(xff, ",")[0])
		if client != "" {
			return m.validSource(client)
		}
	}

	// 检查 X-Real-IP
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		client := strings.TrimSpace(xri)
		if client != "" {
			return m.validSource(client)
		}
	}
	return false
}

// 辅助函数：从字符串解析 IP
func getIP(s string) net.IP {
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		host = s // 可能没有端口
	}
	return net.ParseIP(strings.TrimSpace(host))
}

// 获取 host 部分（可能为纯 IP 或 host:port）
func getHost(s string) string {
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		host = s // 可能没有端口
	}
	return strings.TrimSpace(host)
}
