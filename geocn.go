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
	cacheOnce  sync.Once
	updateOnce sync.Once
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
	return nil
}

func (app *GeoCNApp) Stop() error {
	return nil
}

func (app *GeoCNApp) Provision(ctx caddy.Context) error {
	app.ctx = ctx
	app.lock = new(sync.RWMutex)
	app.logger = ctx.Logger(app)
	app.sfGroup = new(singleflight.Group)

	if app.Source == "" {
		app.Source = remotefile
	}

	caddyDir := caddy.AppDataDir()
	cacheDir := filepath.Join(caddyDir, "geocn")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return fmt.Errorf("failed to create cache directory: %v", err)
	}
	app.localFile = filepath.Join(cacheDir, "Country.mmdb")

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
		app.cache = newIPCache(app.CacheMaxSize, time.Duration(app.CacheTTL))
		app.cacheOnce.Do(func() {
			go app.cache.Cleanup(app.ctx)
		})
		app.logger.Info("IP cache enabled",
			zap.Duration("ttl", time.Duration(app.CacheTTL)),
			zap.Int("max_size", app.CacheMaxSize))
	}

	if err := app.loadDatabase(); err != nil {
		return fmt.Errorf("failed to load GeoIP database: %v", err)
	}

	app.updateOnce.Do(func() {
		go app.periodicUpdate()
	})
	return nil
}

func (app *GeoCNApp) loadDatabase() error {
	if reader, err := geoip2.Open(app.localFile); err == nil {
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
			return fmt.Errorf("local file not found: %s", app.Source)
		}
		if err := copyFile(app.Source, app.localFile); err != nil {
			app.logger.Debug("failed to copy database to cache, using source directly",
				zap.String("source", app.Source),
				zap.Error(err))
			app.localFile = app.Source
		}
	}

	reader, err := geoip2.Open(app.localFile)
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
		return fmt.Errorf("download failed: %v", err)
	}

	tempReader, err := geoip2.Open(tempFile)
	if err != nil {
		if rmErr := os.Remove(tempFile); rmErr != nil {
			app.logger.Debug("failed to remove temp file", zap.String("file", tempFile), zap.Error(rmErr))
		}
		return fmt.Errorf("invalid database file: %v", err)
	}
	tempReader.Close()

	if err := os.Rename(tempFile, app.localFile); err != nil {
		if rmErr := os.Remove(tempFile); rmErr != nil {
			app.logger.Debug("failed to remove temp file", zap.String("file", tempFile), zap.Error(rmErr))
		}
		return fmt.Errorf("replace database file failed: %v", err)
	}

	newReader, err := geoip2.Open(app.localFile)
	if err != nil {
		return fmt.Errorf("open new database file failed: %v", err)
	}

	app.lock.Lock()
	oldReader := app.dbReader
	app.dbReader = newReader
	app.lock.Unlock()

	if oldReader != nil {
		oldReader.Close()
	}

	app.logger.Info("GeoIP database updated successfully", zap.String("file", app.localFile))
	return nil
}

func (app *GeoCNApp) periodicUpdate() {
	if app.Interval == 0 {
		app.Interval = caddy.Duration(24 * time.Hour)
	}

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
	if err != nil || !nip.IsValid() || nip.IsLoopback() || nip.IsLinkLocalUnicast() ||
		nip.IsLinkLocalMulticast() || nip.IsPrivate() || nip.IsUnspecified() || nip.IsMulticast() {
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
	m.logger = ctx.Logger(m)

	appModule, err := ctx.App("geocn")
	if err != nil {
		return fmt.Errorf("failed to get geocn app: %v", err)
	}

	var ok bool
	m.app, ok = appModule.(*GeoCNApp)
	if !ok {
		return fmt.Errorf("geocn app has wrong type")
	}

	return nil
}

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

	var ip, host string

	// Use Caddy's ClientIPVarKey which respects trusted_proxies configuration
	if clientIP, ok := caddyhttp.GetVar(r.Context(), caddyhttp.ClientIPVarKey).(string); ok && clientIP != "" {
		ip = clientIP
		host = getHost(clientIP)
	}

	// Fallback to RemoteAddr if ClientIPVarKey is not set
	if host == "" {
		m.logger.Debug("ClientIPVarKey not set, using RemoteAddr")
		ip = r.RemoteAddr
		host = getHost(r.RemoteAddr)
	}

	if host == "" {
		return false
	}

	country := m.app.lookupCountry(host)
	matched := country == "CN"

	// Set variables for use in Caddyfile (e.g., header directive)
	// Always set these variables so they can be used with {http.vars.geocn_ip}
	caddyhttp.SetVar(r.Context(), "geocn_ip", host)
	caddyhttp.SetVar(r.Context(), "geocn_country", country)

	m.logger.Debug("geocn match result",
		zap.String("client_ip", ip),
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
				if d.NextArg() && d.Val() == "off" {
					enableCache := false
					app.EnableCache = &enableCache
					continue
				}
				enableCache := true
				app.EnableCache = &enableCache
				for d.NextArg() {
					switch d.Val() {
					case "ttl":
						if d.NextArg() {
							val, err := caddy.ParseDuration(d.Val())
							if err != nil {
								return nil, err
							}
							app.CacheTTL = caddy.Duration(val)
						}
					case "size":
						if d.NextArg() {
							var maxSize int
							if _, err := fmt.Sscanf(d.Val(), "%d", &maxSize); err != nil {
								return nil, d.Errf("invalid cache size: %s", d.Val())
							}
							if maxSize > 0 {
								app.CacheMaxSize = maxSize
							}
						}
					}
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
