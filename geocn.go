package geocn

import (
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/oschwald/geoip2-golang/v2"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

var (
	_ caddy.Module                      = (*GeoCN)(nil)
	_ caddyhttp.RequestMatcherWithError = (*GeoCN)(nil)
	_ caddy.Provisioner                 = (*GeoCN)(nil)
	_ caddy.CleanerUpper                = (*GeoCN)(nil)
	_ caddyfile.Unmarshaler             = (*GeoCN)(nil)
)

const remotefile = "https://gh.dev.438250.xyz/https://github.com/Hackl0us/GeoIP2-CN/raw/release/Country.mmdb"

func init() {
	caddy.RegisterModule(GeoCN{})
}

type GeoCN struct {
	Interval     caddy.Duration `json:"interval,omitempty"`
	Timeout      caddy.Duration `json:"timeout,omitempty"`
	Source       string         `json:"source,omitempty"`
	EnableCache  *bool          `json:"enable_cache,omitempty"`
	CacheTTL     caddy.Duration `json:"cache_ttl,omitempty"`
	CacheMaxSize int            `json:"cache_max_size,omitempty"`

	ctx        caddy.Context
	lock       *sync.RWMutex
	dbReader   *geoip2.Reader
	logger     *zap.Logger
	cache      *ipCache
	localFile  string
	httpClient *http.Client
	cacheOnce  *sync.Once
	updateOnce *sync.Once
	sfGroup    *singleflight.Group
}

// ipCache is a TTL cache for IP country lookups.
type ipCache = Cache[string]

// newIPCache creates a new IP cache.
func newIPCache(maxSize int, ttl time.Duration) *ipCache {
	return NewCache[string](maxSize, ttl)
}

func (GeoCN) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.geocn",
		New: func() caddy.Module { return new(GeoCN) },
	}
}

func (m *GeoCN) validSource(host string) bool {
	if host == "" {
		return false
	}

	nip, err := netip.ParseAddr(host)
	if err != nil || !nip.IsValid() || nip.IsLoopback() || nip.IsLinkLocalUnicast() ||
		nip.IsLinkLocalMulticast() || nip.IsPrivate() || nip.IsUnspecified() || nip.IsMulticast() {
		return false
	}

	// Check cache first
	if m.cache != nil {
		if country, found := m.cache.Get(host); found {
			return country == "CN"
		}
	}

	// Use singleflight to deduplicate concurrent requests for the same IP
	result, _, _ := m.sfGroup.Do(host, func() (any, error) {
		// Double-check cache after acquiring singleflight
		if m.cache != nil {
			if country, found := m.cache.Get(host); found {
				return country, nil
			}
		}

		m.lock.RLock()
		defer m.lock.RUnlock()

		if m.dbReader == nil {
			return "", nil
		}

		record, err := m.dbReader.Country(nip)
		if err != nil || record == nil || !record.HasData() {
			return "", nil
		}

		country := record.Country.ISOCode
		if m.cache != nil && country != "" {
			m.cache.Set(host, country)
		}

		return country, nil
	})

	country, _ := result.(string)
	return country == "CN"
}

func (m *GeoCN) Provision(ctx caddy.Context) error {
	m.ctx = ctx
	m.lock = new(sync.RWMutex)
	m.logger = ctx.Logger(m)
	m.cacheOnce = new(sync.Once)
	m.updateOnce = new(sync.Once)
	m.sfGroup = new(singleflight.Group)

	if m.Source == "" {
		m.Source = remotefile
	}

	caddyDir := caddy.AppDataDir()
	cacheDir := filepath.Join(caddyDir, "geocn")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return fmt.Errorf("failed to create cache directory: %v", err)
	}
	m.localFile = filepath.Join(cacheDir, "Country.mmdb")

	if m.Timeout == 0 {
		m.Timeout = caddy.Duration(30 * time.Second)
	}
	m.httpClient = newHTTPClient(time.Duration(m.Timeout))

	if m.EnableCache == nil {
		enableCache := true
		m.EnableCache = &enableCache
	}

	if *m.EnableCache {
		if m.CacheMaxSize <= 0 {
			m.CacheMaxSize = 10000
		}
		if m.CacheTTL == 0 {
			m.CacheTTL = caddy.Duration(5 * time.Minute)
		}
		m.cache = newIPCache(m.CacheMaxSize, time.Duration(m.CacheTTL))
		m.cacheOnce.Do(func() {
			go m.cache.Cleanup(m.ctx)
		})
		m.logger.Info("IP cache enabled",
			zap.Duration("ttl", time.Duration(m.CacheTTL)),
			zap.Int("max_size", m.CacheMaxSize))
	}

	if err := m.loadDatabase(); err != nil {
		return fmt.Errorf("failed to load GeoIP database: %v", err)
	}

	m.updateOnce.Do(func() {
		go m.periodicUpdate()
	})
	return nil
}

func (m *GeoCN) loadDatabase() error {
	if reader, err := geoip2.Open(m.localFile); err == nil {
		m.lock.Lock()
		oldReader := m.dbReader
		m.dbReader = reader
		m.lock.Unlock()
		if oldReader != nil {
			oldReader.Close()
		}
		m.logger.Debug("loaded database from cache",
			zap.String("cache", m.localFile),
			zap.String("source", m.Source))
		return nil
	}

	if isHTTPSource(m.Source) {
		ctx, cancel := getContextWithTimeout(m.ctx, m.Timeout)
		defer cancel()
		if err := downloadFile(ctx, m.httpClient, m.Source, m.localFile); err != nil {
			return fmt.Errorf("download from %s: %w", m.Source, err)
		}
	} else {
		if _, err := os.Stat(m.Source); err != nil {
			return fmt.Errorf("local file not found: %s", m.Source)
		}
		if err := copyFile(m.Source, m.localFile); err != nil {
			m.logger.Debug("failed to copy database to cache, using source directly",
				zap.String("source", m.Source),
				zap.Error(err))
			m.localFile = m.Source
		}
	}

	reader, err := geoip2.Open(m.localFile)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}

	m.lock.Lock()
	oldReader := m.dbReader
	m.dbReader = reader
	m.lock.Unlock()
	if oldReader != nil {
		oldReader.Close()
	}

	m.logger.Info("loaded database",
		zap.String("source", m.Source),
		zap.String("cache", m.localFile))
	return nil
}

func (m *GeoCN) checkNeedUpdate() (bool, error) {
	if !isHTTPSource(m.Source) {
		return false, nil
	}

	ctx, cancel := getContextWithTimeout(m.ctx, m.Timeout)
	defer cancel()
	return checkRemoteUpdate(ctx, m.httpClient, m.Source, m.localFile, time.Duration(m.Interval))
}

func (m *GeoCN) updateGeoFile() error {
	if !isHTTPSource(m.Source) {
		return m.loadDatabase()
	}

	tempFile := m.localFile + ".temp"
	ctx, cancel := getContextWithTimeout(m.ctx, m.Timeout)
	defer cancel()

	if err := downloadFile(ctx, m.httpClient, m.Source, tempFile); err != nil {
		if rmErr := os.Remove(tempFile); rmErr != nil {
			m.logger.Debug("failed to remove temp file", zap.String("file", tempFile), zap.Error(rmErr))
		}
		return fmt.Errorf("download failed: %v", err)
	}

	tempReader, err := geoip2.Open(tempFile)
	if err != nil {
		if rmErr := os.Remove(tempFile); rmErr != nil {
			m.logger.Debug("failed to remove temp file", zap.String("file", tempFile), zap.Error(rmErr))
		}
		return fmt.Errorf("invalid database file: %v", err)
	}
	tempReader.Close()

	if err := os.Rename(tempFile, m.localFile); err != nil {
		if rmErr := os.Remove(tempFile); rmErr != nil {
			m.logger.Debug("failed to remove temp file", zap.String("file", tempFile), zap.Error(rmErr))
		}
		return fmt.Errorf("replace database file failed: %v", err)
	}

	newReader, err := geoip2.Open(m.localFile)
	if err != nil {
		return fmt.Errorf("open new database file failed: %v", err)
	}

	m.lock.Lock()
	oldReader := m.dbReader
	m.dbReader = newReader
	m.lock.Unlock()

	if oldReader != nil {
		oldReader.Close()
	}

	m.logger.Info("GeoIP database updated successfully", zap.String("file", m.localFile))
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
				if d.NextArg() && d.Val() == "off" {
					enableCache := false
					m.EnableCache = &enableCache
					continue
				}
				enableCache := true
				m.EnableCache = &enableCache
				for d.NextArg() {
					switch d.Val() {
					case "ttl":
						if d.NextArg() {
							val, err := caddy.ParseDuration(d.Val())
							if err != nil {
								return err
							}
							m.CacheTTL = caddy.Duration(val)
						}
					case "size":
						if d.NextArg() {
							var maxSize int
							if _, err := fmt.Sscanf(d.Val(), "%d", &maxSize); err != nil {
								return d.Errf("invalid cache size: %s", d.Val())
							}
							if maxSize > 0 {
								m.CacheMaxSize = maxSize
							}
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

func (m *GeoCN) MatchWithError(r *http.Request) (bool, error) {
	return m.Match(r), nil
}

func (m *GeoCN) Match(r *http.Request) bool {
	// Use Caddy's ClientIPVarKey which respects trusted_proxies configuration
	if clientIP, ok := caddyhttp.GetVar(r.Context(), caddyhttp.ClientIPVarKey).(string); ok && clientIP != "" {
		if host := getHost(clientIP); host != "" {
			matched := m.validSource(host)
			m.logger.Debug("geocn match result",
				zap.String("client_ip", clientIP),
				zap.Bool("is_cn", matched))
			return matched
		}
	}
	m.logger.Debug("ClientIPVarKey not set, using RemoteAddr")
	// Fallback to RemoteAddr if ClientIPVarKey is not set
	if host := getHost(r.RemoteAddr); host != "" {
		matched := m.validSource(host)
		m.logger.Debug("geocn match result (fallback)",
			zap.String("remote_addr", r.RemoteAddr),
			zap.String("host", host),
			zap.Bool("is_cn", matched))
		return matched
	}

	return false
}
