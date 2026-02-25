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
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/oschwald/geoip2-golang/v2"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

var (
	_ caddy.Module                      = (*GeoCN)(nil)
	_ caddyhttp.RequestMatcherWithError = (*GeoCN)(nil)
	_ caddy.Provisioner                 = (*GeoCN)(nil)
	_ caddyfile.Unmarshaler             = (*GeoCN)(nil)

	_ caddy.Module       = (*GeoCNApp)(nil)
	_ caddy.App          = (*GeoCNApp)(nil)
	_ caddy.Provisioner  = (*GeoCNApp)(nil)
	_ caddy.Validator    = (*GeoCNApp)(nil)
	_ caddy.CleanerUpper = (*GeoCNApp)(nil)
)

const remotefile = "https://gh.dev.438250.xyz/https://github.com/Hackl0us/GeoIP2-CN/raw/release/Country.mmdb"

func init() {
	caddy.RegisterModule(GeoCNApp{})
	caddy.RegisterModule(GeoCN{})
	httpcaddyfile.RegisterGlobalOption("geocn", parseGeoCNAppCaddyfile)
}

// GeoCNApp is the global app module that manages shared GeoIP resources.
type GeoCNApp struct {
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
	sfGroup    *singleflight.Group
}

// GeoCN is a lightweight matcher that references the global GeoCNApp.
type GeoCN struct {
	app    *GeoCNApp
	logger *zap.Logger
}

// ipCache is a TTL cache for IP country lookups.
type ipCache = Cache[string]

// newIPCache creates a new IP cache.
func newIPCache(maxSize int, ttl time.Duration) *ipCache {
	return NewCache[string](maxSize, ttl)
}

func (GeoCNApp) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "geocn",
		New: func() caddy.Module { return new(GeoCNApp) },
	}
}

func (GeoCN) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.geocn",
		New: func() caddy.Module { return new(GeoCN) },
	}
}

func (app *GeoCNApp) Start() error {
	// Create cache directory in Start() to avoid side effects during caddy validate
	caddyDir := caddy.AppDataDir()
	cacheDir := filepath.Join(caddyDir, "geocn")
	if err := os.MkdirAll(cacheDir, 0750); err != nil {
		return fmt.Errorf("failed to create cache directory: %w", err)
	}
	app.localFile = filepath.Join(cacheDir, "Country.mmdb")

	if app.EnableCache != nil && *app.EnableCache {
		app.cache = newIPCache(app.CacheMaxSize, time.Duration(app.CacheTTL))
	}

	if err := app.loadDatabase(); err != nil {
		return fmt.Errorf("failed to load GeoIP database: %w", err)
	}

	// Only start background goroutines after all error checks pass
	if app.cache != nil {
		go app.cache.Cleanup(app.ctx)
	}
	go app.periodicUpdate()
	return nil
}

func (app *GeoCNApp) Stop() error {
	return nil
}

func (app *GeoCNApp) Provision(ctx caddy.Context) error {
	app.ctx = ctx
	app.lock = new(sync.RWMutex)
	app.logger = ctx.Logger()
	app.sfGroup = new(singleflight.Group)

	if app.Source == "" {
		app.Source = remotefile
	}

	if app.Timeout == 0 {
		app.Timeout = caddy.Duration(30 * time.Second)
	}
	app.httpClient = newHTTPClient(time.Duration(app.Timeout))

	if app.EnableCache == nil {
		enableCache := true
		app.EnableCache = &enableCache
	}

	if *app.EnableCache {
		if app.CacheMaxSize <= 0 {
			app.CacheMaxSize = 10000
		}
		if app.CacheTTL == 0 {
			app.CacheTTL = caddy.Duration(5 * time.Minute)
		}
		app.logger.Info("IP cache enabled",
			zap.Duration("ttl", time.Duration(app.CacheTTL)),
			zap.Int("max_size", app.CacheMaxSize))
	}

	if app.Interval == 0 {
		app.Interval = caddy.Duration(24 * time.Hour)
	}

	return nil
}

// openGeoIPFromFile loads a mmdb file entirely into memory and returns a Reader.
// This avoids holding file handles open, which prevents os.Rename failures on Windows.
func openGeoIPFromFile(path string) (*geoip2.Reader, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return geoip2.OpenBytes(data)
}

func (app *GeoCNApp) loadDatabase() error {
	if reader, err := openGeoIPFromFile(app.localFile); err == nil {
		app.lock.Lock()
		oldReader := app.dbReader
		app.dbReader = reader
		app.lock.Unlock()
		if oldReader != nil {
			oldReader.Close()
		}
		app.logger.Debug("loaded database from cache",
			zap.String("cache", app.localFile),
			zap.String("source", app.Source))
		return nil
	}

	if isHTTPSource(app.Source) {
		ctx, cancel := getContextWithTimeout(app.ctx, app.Timeout)
		defer cancel()
		if err := downloadFile(ctx, app.httpClient, app.Source, app.localFile); err != nil {
			return fmt.Errorf("download from %s: %w", app.Source, err)
		}
	} else {
		if _, err := os.Stat(app.Source); err != nil {
			return fmt.Errorf("local file not found: %w", err)
		}
		if err := copyFile(app.Source, app.localFile); err != nil {
			app.logger.Debug("failed to copy database to cache, using source directly",
				zap.String("source", app.Source),
				zap.Error(err))
			app.localFile = app.Source
		}
	}

	reader, err := openGeoIPFromFile(app.localFile)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}

	app.lock.Lock()
	oldReader := app.dbReader
	app.dbReader = reader
	app.lock.Unlock()
	if oldReader != nil {
		oldReader.Close()
	}

	app.logger.Info("loaded database",
		zap.String("source", app.Source),
		zap.String("cache", app.localFile))
	return nil
}

func (app *GeoCNApp) checkNeedUpdate() (bool, error) {
	if !isHTTPSource(app.Source) {
		return false, nil
	}

	ctx, cancel := getContextWithTimeout(app.ctx, app.Timeout)
	defer cancel()
	return checkRemoteUpdate(ctx, app.httpClient, app.Source, app.localFile, time.Duration(app.Interval))
}

func (app *GeoCNApp) updateGeoFile() error {
	if !isHTTPSource(app.Source) {
		return app.loadDatabase()
	}

	tempFile := app.localFile + ".temp"
	ctx, cancel := getContextWithTimeout(app.ctx, app.Timeout)
	defer cancel()

	if err := downloadFile(ctx, app.httpClient, app.Source, tempFile); err != nil {
		if rmErr := os.Remove(tempFile); rmErr != nil {
			app.logger.Debug("failed to remove temp file", zap.String("file", tempFile), zap.Error(rmErr))
		}
		return fmt.Errorf("download failed: %w", err)
	}

	// Validate by loading into memory — no file handle held after this
	tempReader, err := openGeoIPFromFile(tempFile)
	if err != nil {
		if rmErr := os.Remove(tempFile); rmErr != nil {
			app.logger.Debug("failed to remove temp file", zap.String("file", tempFile), zap.Error(rmErr))
		}
		return fmt.Errorf("invalid database file: %w", err)
	}

	if err := os.Rename(tempFile, app.localFile); err != nil {
		tempReader.Close()
		if rmErr := os.Remove(tempFile); rmErr != nil {
			app.logger.Debug("failed to remove temp file", zap.String("file", tempFile), zap.Error(rmErr))
		}
		return fmt.Errorf("replace database file failed: %w", err)
	}

	// Swap the already-loaded reader directly — no need to re-open from file
	app.lock.Lock()
	oldReader := app.dbReader
	app.dbReader = tempReader
	app.lock.Unlock()

	if oldReader != nil {
		oldReader.Close()
	}

	app.logger.Info("GeoIP database updated successfully", zap.String("file", app.localFile))
	return nil
}

func (app *GeoCNApp) periodicUpdate() {
	ticker := time.NewTicker(time.Duration(app.Interval))
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if ok, err := app.checkNeedUpdate(); err != nil {
				app.logger.Warn("check update failed", zap.Error(err))
			} else if ok {
				if err := app.updateGeoFile(); err != nil {
					app.logger.Error("update database failed", zap.Error(err))
				}
			}
		case <-app.ctx.Done():
			return
		}
	}
}

// Validate implements caddy.Validator.
func (app *GeoCNApp) Validate() error {
	if app.Interval <= 0 {
		return fmt.Errorf("geocn: interval must be positive")
	}
	if app.Source != "" && !isHTTPSource(app.Source) {
		if _, err := os.Stat(app.Source); err != nil {
			return fmt.Errorf("geocn: source file not found: %s", app.Source)
		}
	}
	return nil
}

func (app *GeoCNApp) Cleanup() error {
	app.lock.Lock()
	defer app.lock.Unlock()

	if app.dbReader != nil {
		err := app.dbReader.Close()
		app.dbReader = nil
		return err
	}
	return nil
}

func (app *GeoCNApp) lookupCountry(host string) string {
	nip, err := netip.ParseAddr(host)
	if err != nil || !nip.IsValid() || checkPrivateAddr(nip) {
		return ""
	}

	// Check cache first
	if app.cache != nil {
		if country, found := app.cache.Get(host); found {
			return country
		}
	}

	// Use singleflight to deduplicate concurrent requests for the same IP
	result, _, _ := app.sfGroup.Do(host, func() (any, error) {
		// Double-check cache after acquiring singleflight
		if app.cache != nil {
			if country, found := app.cache.Get(host); found {
				return country, nil
			}
		}

		app.lock.RLock()
		defer app.lock.RUnlock()

		if app.dbReader == nil {
			return "", nil
		}

		record, err := app.dbReader.Country(nip)
		if err != nil || record == nil || !record.HasData() {
			return "", nil
		}

		country := record.Country.ISOCode
		if app.cache != nil && country != "" {
			app.cache.Set(host, country)
		}

		return country, nil
	})

	country, _ := result.(string)
	return country
}

func (m *GeoCN) Provision(ctx caddy.Context) error {
	m.logger = ctx.Logger()

	appModule, err := ctx.App("geocn")
	if err != nil {
		return fmt.Errorf("failed to get geocn app: %w", err)
	}

	var ok bool
	m.app, ok = appModule.(*GeoCNApp)
	if !ok {
		return fmt.Errorf("geocn app has wrong type")
	}

	return nil
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
// The geocn matcher has no configuration of its own; all settings
// are on the global geocn app. Syntax:
//
//	geocn
func (m *GeoCN) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	// The matcher itself has no configuration - all config is on the app
	d.Next()
	return nil
}

func (m *GeoCN) MatchWithError(r *http.Request) (bool, error) {
	return m.Match(r), nil
}

func (m *GeoCN) Match(r *http.Request) bool {
	if m.app == nil {
		m.logger.Error("geocn app not initialized")
		return false
	}

	host, raw := extractClientIP(r)
	if host == "" {
		return false
	}

	country := m.app.lookupCountry(host)
	matched := country == "CN"

	m.logger.Debug("geocn match result",
		zap.String("client_ip", raw),
		zap.String("country", country),
		zap.Bool("is_cn", matched))

	return matched
}

// parseGeoCNAppCaddyfile parses the global geocn option.
//
//	{
//	    geocn {
//	        interval 24h
//	        timeout 30s
//	        source https://example.com/Country.mmdb
//	        cache ttl 5m size 10000
//	        # or: cache off
//	    }
//	}
func parseGeoCNAppCaddyfile(d *caddyfile.Dispenser, _ any) (any, error) {
	app := new(GeoCNApp)

	for d.Next() {
		for n := d.Nesting(); d.NextBlock(n); {
			switch d.Val() {
			case "interval":
				if !d.NextArg() {
					return nil, d.ArgErr()
				}
				val, err := caddy.ParseDuration(d.Val())
				if err != nil {
					return nil, err
				}
				app.Interval = caddy.Duration(val)
			case "timeout":
				if !d.NextArg() {
					return nil, d.ArgErr()
				}
				val, err := caddy.ParseDuration(d.Val())
				if err != nil {
					return nil, err
				}
				app.Timeout = caddy.Duration(val)
			case "source":
				if !d.NextArg() {
					return nil, d.ArgErr()
				}
				app.Source = d.Val()
			case "cache":
				if err := parseCacheBlock(d, &app.EnableCache, &app.CacheTTL, &app.CacheMaxSize); err != nil {
					return nil, err
				}
				if app.EnableCache != nil && !*app.EnableCache {
					continue
				}
			default:
				return nil, d.ArgErr()
			}
		}
	}

	return httpcaddyfile.App{
		Name:  "geocn",
		Value: caddyconfig.JSON(app, nil),
	}, nil
}
