package geocn

import (
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/lionsoul2014/ip2region/binding/golang/xdb"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

var (
	_ caddy.Module                      = (*GeoCity)(nil)
	_ caddyhttp.RequestMatcherWithError = (*GeoCity)(nil)
	_ caddy.Provisioner                 = (*GeoCity)(nil)
	_ caddyfile.Unmarshaler             = (*GeoCity)(nil)

	_ caddy.Module       = (*GeoCityApp)(nil)
	_ caddy.App          = (*GeoCityApp)(nil)
	_ caddy.Provisioner  = (*GeoCityApp)(nil)
	_ caddy.Validator    = (*GeoCityApp)(nil)
	_ caddy.CleanerUpper = (*GeoCityApp)(nil)
)

const (
	ip2regionIPv4RemoteFile = "https://gh.dev.438250.xyz/https://github.com/lionsoul2014/ip2region/raw/master/data/ip2region_v4.xdb"
	ip2regionIPv6RemoteFile = "https://gh.dev.438250.xyz/https://github.com/lionsoul2014/ip2region/raw/master/data/ip2region_v6.xdb"
)

func init() {
	caddy.RegisterModule(GeoCityApp{})
	caddy.RegisterModule(GeoCity{})
	httpcaddyfile.RegisterGlobalOption("geocity", parseGeoCityAppCaddyfile)
}

// GeoCityApp is the global app module that manages shared ip2region resources.
type GeoCityApp struct {
	Interval     caddy.Duration `json:"interval,omitempty"`
	Timeout      caddy.Duration `json:"timeout,omitempty"`
	IPv4Source   string         `json:"ipv4_source,omitempty"`
	IPv6Source   string         `json:"ipv6_source,omitempty"`
	EnableCache  *bool          `json:"enable_cache,omitempty"`
	CacheTTL     caddy.Duration `json:"cache_ttl,omitempty"`
	CacheMaxSize int            `json:"cache_max_size,omitempty"`

	ctx           caddy.Context
	lock          *sync.RWMutex
	searcherIPv4  *xdb.Searcher
	searcherIPv6  *xdb.Searcher
	localIPv4File string
	localIPv6File string
	logger        *zap.Logger
	cache         *cityCache
	httpClient    *http.Client
	sfGroup       *singleflight.Group
}

// GeoCity is a lightweight matcher that references the global GeoCityApp.
type GeoCity struct {
	Regions   []string `json:"regions,omitempty"`
	Provinces []string `json:"provinces,omitempty"` // Deprecated: use Regions instead
	Cities    []string `json:"cities,omitempty"`    // Deprecated: use Regions instead

	app         *GeoCityApp
	logger      *zap.Logger
	allKeywords []string // pre-merged keywords from Regions+Provinces+Cities
}

// cityCache is a TTL cache for IP region lookups.
type cityCache = Cache[string]

// newCityCache creates a new city cache.
func newCityCache(maxSize int, ttl time.Duration) *cityCache {
	return NewCache[string](maxSize, ttl)
}

func (GeoCityApp) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "geocity",
		New: func() caddy.Module { return new(GeoCityApp) },
	}
}

func (GeoCity) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.geocity",
		New: func() caddy.Module { return new(GeoCity) },
	}
}

func (app *GeoCityApp) Provision(ctx caddy.Context) error {
	app.ctx = ctx
	app.lock = new(sync.RWMutex)
	app.logger = ctx.Logger()
	app.sfGroup = new(singleflight.Group)

	if app.Timeout == 0 {
		app.Timeout = caddy.Duration(30 * time.Second)
	}
	app.httpClient = newHTTPClient(time.Duration(app.Timeout))

	if app.IPv4Source == "" {
		app.IPv4Source = ip2regionIPv4RemoteFile
	}
	if app.IPv6Source == "" {
		app.IPv6Source = ip2regionIPv6RemoteFile
	}

	if app.Interval == 0 {
		app.Interval = caddy.Duration(24 * time.Hour)
	}

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
		app.logger.Info("City cache enabled",
			zap.Duration("ttl", time.Duration(app.CacheTTL)),
			zap.Int("max_size", app.CacheMaxSize))
	}

	return nil
}

// Validate implements caddy.Validator.
func (app *GeoCityApp) Validate() error {
	if app.Interval <= 0 {
		return fmt.Errorf("geocity: interval must be positive")
	}
	if app.IPv4Source != "" && !isHTTPSource(app.IPv4Source) {
		if _, err := os.Stat(app.IPv4Source); err != nil {
			return fmt.Errorf("geocity: ipv4_source file not found: %s", app.IPv4Source)
		}
	}
	if app.IPv6Source != "" && !isHTTPSource(app.IPv6Source) {
		if _, err := os.Stat(app.IPv6Source); err != nil {
			return fmt.Errorf("geocity: ipv6_source file not found: %s", app.IPv6Source)
		}
	}
	return nil
}

func (app *GeoCityApp) Start() error {
	// Create cache directory in Start() to avoid side effects during caddy validate
	caddyDir := caddy.AppDataDir()
	cacheDir := filepath.Join(caddyDir, "geocity")
	if err := os.MkdirAll(cacheDir, 0750); err != nil {
		return fmt.Errorf("failed to create cache directory: %w", err)
	}
	app.localIPv4File = filepath.Join(cacheDir, "ipv4.xdb")
	app.localIPv6File = filepath.Join(cacheDir, "ipv6.xdb")

	if app.EnableCache != nil && *app.EnableCache {
		app.cache = newCityCache(app.CacheMaxSize, time.Duration(app.CacheTTL))
	}

	if err := app.loadDatabase(app.IPv4Source, app.localIPv4File, xdb.IPv4, &app.searcherIPv4); err != nil {
		app.logger.Warn("failed to load IPv4 database",
			zap.String("source", app.IPv4Source),
			zap.Error(err))
	}

	if err := app.loadDatabase(app.IPv6Source, app.localIPv6File, xdb.IPv6, &app.searcherIPv6); err != nil {
		app.logger.Warn("failed to load IPv6 database",
			zap.String("source", app.IPv6Source),
			zap.Error(err))
	}

	if app.searcherIPv4 == nil && app.searcherIPv6 == nil {
		return fmt.Errorf("failed to load any IP database (neither IPv4 nor IPv6)")
	}

	// Only start background goroutines after all error checks pass
	if app.cache != nil {
		go app.cache.Cleanup(app.ctx)
	}
	go app.periodicUpdate()
	return nil
}

func (app *GeoCityApp) Stop() error {
	return nil
}

func (app *GeoCityApp) Cleanup() error {
	app.lock.Lock()
	defer app.lock.Unlock()

	if app.searcherIPv4 != nil {
		app.searcherIPv4.Close()
		app.searcherIPv4 = nil
	}
	if app.searcherIPv6 != nil {
		app.searcherIPv6.Close()
		app.searcherIPv6 = nil
	}
	return nil
}

// openXDBFromFile loads an xdb file entirely into memory and returns a Searcher.
// This avoids holding file handles open, which prevents os.Rename failures on Windows.
func openXDBFromFile(version *xdb.Version, path string) (*xdb.Searcher, error) {
	data, err := xdb.LoadContentFromFile(path)
	if err != nil {
		return nil, err
	}
	return xdb.NewWithBuffer(version, data)
}

func (app *GeoCityApp) loadDatabase(source, cacheFile string, version *xdb.Version, searcher **xdb.Searcher) error {
	if s, err := openXDBFromFile(version, cacheFile); err == nil {
		app.lock.Lock()
		oldSearcher := *searcher
		*searcher = s
		app.lock.Unlock()
		if oldSearcher != nil {
			oldSearcher.Close()
		}
		app.logger.Debug("loaded database from cache",
			zap.String("cache", cacheFile),
			zap.String("source", source))
		return nil
	}

	if isHTTPSource(source) {
		ctx, cancel := getContextWithTimeout(app.ctx, app.Timeout)
		defer cancel()
		if err := downloadFile(ctx, app.httpClient, source, cacheFile); err != nil {
			return fmt.Errorf("download from %s: %w", source, err)
		}
	} else {
		if _, err := os.Stat(source); err != nil {
			return fmt.Errorf("local file not found: %w", err)
		}
		if err := copyFile(source, cacheFile); err != nil {
			app.logger.Debug("failed to copy database to cache, using source directly",
				zap.String("source", source),
				zap.Error(err))
			// Update struct field when using source directly
			if version == xdb.IPv4 {
				app.localIPv4File = source
			} else {
				app.localIPv6File = source
			}
			cacheFile = source
		}
	}

	s, err := openXDBFromFile(version, cacheFile)
	if err != nil {
		return fmt.Errorf("load database: %w", err)
	}
	app.lock.Lock()
	oldSearcher := *searcher
	*searcher = s
	app.lock.Unlock()
	if oldSearcher != nil {
		oldSearcher.Close()
	}

	app.logger.Info("loaded database",
		zap.String("source", source),
		zap.String("cache", cacheFile))
	return nil
}

func (app *GeoCityApp) updateDatabase(source, localFile string, version *xdb.Version, searcher **xdb.Searcher, label string) error {
	if !isHTTPSource(source) {
		return app.loadDatabase(source, localFile, version, searcher)
	}

	tempFile := localFile + ".temp"
	ctx, cancel := getContextWithTimeout(app.ctx, app.Timeout)
	defer cancel()

	if err := downloadFile(ctx, app.httpClient, source, tempFile); err != nil {
		if rmErr := os.Remove(tempFile); rmErr != nil {
			app.logger.Debug("failed to remove temp file", zap.String("file", tempFile), zap.Error(rmErr))
		}
		return fmt.Errorf("download %s database failed: %w", label, err)
	}

	// Validate by loading into memory — no file handle held after this
	tempSearcher, err := openXDBFromFile(version, tempFile)
	if err != nil {
		if rmErr := os.Remove(tempFile); rmErr != nil {
			app.logger.Debug("failed to remove temp file", zap.String("file", tempFile), zap.Error(rmErr))
		}
		return fmt.Errorf("invalid %s database file: %w", label, err)
	}

	if err := os.Rename(tempFile, localFile); err != nil {
		tempSearcher.Close()
		if rmErr := os.Remove(tempFile); rmErr != nil {
			app.logger.Debug("failed to remove temp file", zap.String("file", tempFile), zap.Error(rmErr))
		}
		return fmt.Errorf("replace %s database file failed: %w", label, err)
	}

	// Swap the already-loaded searcher directly — no need to re-open from file
	app.lock.Lock()
	oldSearcher := *searcher
	*searcher = tempSearcher
	app.lock.Unlock()

	if oldSearcher != nil {
		oldSearcher.Close()
	}

	app.logger.Info(label+" database updated successfully", zap.String("file", localFile))
	return nil
}

func (app *GeoCityApp) updateDatabaseIPv4() error {
	return app.updateDatabase(app.IPv4Source, app.localIPv4File, xdb.IPv4, &app.searcherIPv4, "IPv4")
}

func (app *GeoCityApp) updateDatabaseIPv6() error {
	return app.updateDatabase(app.IPv6Source, app.localIPv6File, xdb.IPv6, &app.searcherIPv6, "IPv6")
}

func (app *GeoCityApp) periodicUpdate() {
	ticker := time.NewTicker(time.Duration(app.Interval))
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			app.tryUpdateSource(app.IPv4Source, app.localIPv4File, app.updateDatabaseIPv4, "IPv4")
			app.tryUpdateSource(app.IPv6Source, app.localIPv6File, app.updateDatabaseIPv6, "IPv6")
		case <-app.ctx.Done():
			return
		}
	}
}

func (app *GeoCityApp) tryUpdateSource(source, localFile string, updateFn func() error, label string) {
	if source == "" || !isHTTPSource(source) {
		return
	}

	ctx, cancel := getContextWithTimeout(app.ctx, app.Timeout)
	defer cancel()

	ok, err := checkRemoteUpdate(ctx, app.httpClient, source, localFile, time.Duration(app.Interval))
	if err != nil {
		app.logger.Warn("check "+label+" update failed", zap.Error(err))
		return
	}
	if ok {
		if err := updateFn(); err != nil {
			app.logger.Error("update "+label+" database failed", zap.Error(err))
		}
	}
}

// lookupRegion returns the region string for an IP.
// It only caches the region string so different matchers with different
// region filters can share the same cache safely.
func (app *GeoCityApp) lookupRegion(host string) string {
	nip, err := netip.ParseAddr(host)
	if err != nil || !nip.IsValid() || checkPrivateAddr(nip) {
		return ""
	}

	// Check cache first
	if app.cache != nil {
		if region, found := app.cache.Get(host); found {
			return region
		}
	}

	// Use singleflight to deduplicate concurrent requests for the same IP
	result, _, _ := app.sfGroup.Do(host, func() (any, error) {
		// Double-check cache after acquiring singleflight
		if app.cache != nil {
			if region, found := app.cache.Get(host); found {
				return region, nil
			}
		}

		app.lock.RLock()
		defer app.lock.RUnlock()

		var searcher *xdb.Searcher
		if nip.Is4() || nip.Is4In6() {
			searcher = app.searcherIPv4
			if searcher == nil {
				return "", nil
			}
		} else {
			searcher = app.searcherIPv6
			if searcher == nil {
				return "", nil
			}
		}

		region, err := searcher.SearchByStr(host)
		if err != nil {
			app.logger.Debug("failed to search IP location", zap.String("ip", host), zap.Error(err))
			return "", nil
		}

		if app.cache != nil && region != "" {
			app.cache.Set(host, region)
		}

		return region, nil
	})

	region, _ := result.(string)
	return region
}

// --- GeoCity matcher ---

func (g *GeoCity) Provision(ctx caddy.Context) error {
	g.logger = ctx.Logger()

	if len(g.Provinces) > 0 {
		g.logger.Warn("'provinces' is deprecated, use 'regions' instead")
	}
	if len(g.Cities) > 0 {
		g.logger.Warn("'cities' is deprecated, use 'regions' instead")
	}

	// Pre-merge all keywords to avoid per-request allocation
	g.allKeywords = make([]string, 0, len(g.Regions)+len(g.Provinces)+len(g.Cities))
	g.allKeywords = append(g.allKeywords, g.Regions...)
	g.allKeywords = append(g.allKeywords, g.Provinces...)
	g.allKeywords = append(g.allKeywords, g.Cities...)

	appModule, err := ctx.App("geocity")
	if err != nil {
		return fmt.Errorf("failed to get geocity app: %w", err)
	}

	var ok bool
	g.app, ok = appModule.(*GeoCityApp)
	if !ok {
		return fmt.Errorf("geocity app has wrong type")
	}

	return nil
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler. Syntax:
//
//	geocity {
//	    regions       <keyword> [<keyword>...]
//	    provinces     <keyword> [<keyword>...]
//	    cities        <keyword> [<keyword>...]
//	}
func (g *GeoCity) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for n := d.Nesting(); d.NextBlock(n); {
			switch d.Val() {
			case "regions":
				args := d.RemainingArgs()
				if len(args) == 0 {
					return d.ArgErr()
				}
				g.Regions = append(g.Regions, args...)
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
			default:
				return d.ArgErr()
			}
		}
	}
	return nil
}

// matchRegion checks if the region string contains any of the configured keywords.
// It searches in the full region string (e.g., "中国|0|北京|北京市|联通").
func (g *GeoCity) matchRegion(region string) bool {
	if len(g.allKeywords) == 0 {
		return true // No filter configured, match all Chinese IPs
	}

	for _, kw := range g.allKeywords {
		if strings.Contains(region, kw) {
			return true
		}
	}

	return false
}

func (g *GeoCity) MatchWithError(r *http.Request) (bool, error) {
	return g.Match(r), nil
}

func (g *GeoCity) Match(r *http.Request) bool {
	if g.app == nil {
		g.logger.Error("geocity app not initialized")
		return false
	}

	host, raw := extractClientIP(r)
	if host == "" {
		return false
	}

	// Lookup region, then match locally per-matcher config
	region := g.app.lookupRegion(host)

	var matched bool
	if region == "" {
		matched = false
	} else {
		country, _, _ := strings.Cut(region, "|")
		country = strings.TrimSpace(country)
		matched = country == "中国" && g.matchRegion(region)
	}

	g.logger.Debug("geocity match result",
		zap.String("client_ip", raw),
		zap.String("region", region),
		zap.Bool("matched", matched))

	return matched
}

// parseGeoCityAppCaddyfile parses the global geocity option.
//
//	{
//	    geocity {
//	        interval 24h
//	        timeout 30s
//	        ipv4_source <url_or_path>
//	        ipv6_source <url_or_path>
//	        cache ttl 5m size 10000
//	        # or: cache off
//	    }
//	}
func parseGeoCityAppCaddyfile(d *caddyfile.Dispenser, _ any) (any, error) {
	app := new(GeoCityApp)

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
			case "ipv4_source":
				if !d.NextArg() {
					return nil, d.ArgErr()
				}
				app.IPv4Source = d.Val()
			case "ipv6_source":
				if !d.NextArg() {
					return nil, d.ArgErr()
				}
				app.IPv6Source = d.Val()
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
		Name:  "geocity",
		Value: caddyconfig.JSON(app, nil),
	}, nil
}
