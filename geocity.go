package geocn

import (
	"fmt"
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
	"golang.org/x/sync/singleflight"
)

var (
	_ caddy.Module                      = (*GeoCity)(nil)
	_ caddyhttp.RequestMatcherWithError = (*GeoCity)(nil)
	_ caddy.Provisioner                 = (*GeoCity)(nil)
	_ caddy.CleanerUpper                = (*GeoCity)(nil)
	_ caddyfile.Unmarshaler             = (*GeoCity)(nil)
)

const (
	ip2regionIPv4RemoteFile = "https://gh.dev.438250.xyz/https://github.com/lionsoul2014/ip2region/raw/master/data/ip2region_v4.xdb"
	ip2regionIPv6RemoteFile = "https://gh.dev.438250.xyz/https://github.com/lionsoul2014/ip2region/raw/master/data/ip2region_v6.xdb"
)

func init() {
	caddy.RegisterModule(GeoCity{})
}

type GeoCity struct {
	Interval     caddy.Duration `json:"interval,omitempty"`
	Timeout      caddy.Duration `json:"timeout,omitempty"`
	IPv4Source   string         `json:"ipv4_source,omitempty"`
	IPv6Source   string         `json:"ipv6_source,omitempty"`
	Regions      []string       `json:"regions,omitempty"`
	Provinces    []string       `json:"provinces,omitempty"` // Deprecated: use Regions instead
	Cities       []string       `json:"cities,omitempty"`    // Deprecated: use Regions instead
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
	cacheOnce     *sync.Once
	updateOnce    *sync.Once
	sfGroup       *singleflight.Group
}

// cityResult holds the cached result for a city lookup.
type cityResult struct {
	Region  string
	Matched bool
}

// cityCache is a TTL cache for city match results.
type cityCache = Cache[cityResult]

// newCityCache creates a new city cache.
func newCityCache(maxSize int, ttl time.Duration) *cityCache {
	return NewCache[cityResult](maxSize, ttl)
}

func (GeoCity) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.geocity",
		New: func() caddy.Module { return new(GeoCity) },
	}
}

func (g *GeoCity) Provision(ctx caddy.Context) error {
	g.ctx = ctx
	g.lock = new(sync.RWMutex)
	g.logger = ctx.Logger(g)
	g.cacheOnce = new(sync.Once)
	g.updateOnce = new(sync.Once)
	g.sfGroup = new(singleflight.Group)

	if g.Timeout == 0 {
		g.Timeout = caddy.Duration(30 * time.Second)
	}
	g.httpClient = newHTTPClient(time.Duration(g.Timeout))

	caddyDir := caddy.AppDataDir()
	cacheDir := filepath.Join(caddyDir, "geocity")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return fmt.Errorf("failed to create cache directory: %v", err)
	}

	g.localIPv4File = filepath.Join(cacheDir, "ipv4.xdb")
	g.localIPv6File = filepath.Join(cacheDir, "ipv6.xdb")

	if g.IPv4Source == "" {
		g.IPv4Source = ip2regionIPv4RemoteFile
	}
	if g.IPv6Source == "" {
		g.IPv6Source = ip2regionIPv6RemoteFile
	}

	if g.EnableCache == nil {
		enableCache := true
		g.EnableCache = &enableCache
	}

	if *g.EnableCache {
		if g.CacheMaxSize <= 0 {
			g.CacheMaxSize = 10000
		}
		if g.CacheTTL == 0 {
			g.CacheTTL = caddy.Duration(5 * time.Minute)
		}
		g.cache = newCityCache(g.CacheMaxSize, time.Duration(g.CacheTTL))
		g.cacheOnce.Do(func() {
			go g.cache.Cleanup(g.ctx)
		})
		g.logger.Info("City cache enabled",
			zap.Duration("ttl", time.Duration(g.CacheTTL)),
			zap.Int("max_size", g.CacheMaxSize))
	}

	if err := g.loadDatabase(g.IPv4Source, g.localIPv4File, xdb.IPv4, &g.searcherIPv4); err != nil {
		g.logger.Warn("failed to load IPv4 database",
			zap.String("source", g.IPv4Source),
			zap.Error(err))
	}

	if err := g.loadDatabase(g.IPv6Source, g.localIPv6File, xdb.IPv6, &g.searcherIPv6); err != nil {
		g.logger.Warn("failed to load IPv6 database",
			zap.String("source", g.IPv6Source),
			zap.Error(err))
	}

	if g.searcherIPv4 == nil && g.searcherIPv6 == nil {
		return fmt.Errorf("failed to load any IP database (neither IPv4 nor IPv6)")
	}

	g.updateOnce.Do(func() {
		go g.periodicUpdate()
	})
	return nil
}

func (g *GeoCity) loadDatabase(source, cacheFile string, version *xdb.Version, searcher **xdb.Searcher) error {
	if s, err := xdb.NewWithFileOnly(version, cacheFile); err == nil {
		g.lock.Lock()
		oldSearcher := *searcher
		*searcher = s
		g.lock.Unlock()
		if oldSearcher != nil {
			oldSearcher.Close()
		}
		g.logger.Debug("loaded database from cache",
			zap.String("cache", cacheFile),
			zap.String("source", source))
		return nil
	}

	if isHTTPSource(source) {
		ctx, cancel := getContextWithTimeout(g.ctx, g.Timeout)
		defer cancel()
		if err := downloadFile(ctx, g.httpClient, source, cacheFile); err != nil {
			return fmt.Errorf("download from %s: %w", source, err)
		}
	} else {
		if _, err := os.Stat(source); err != nil {
			return fmt.Errorf("local file not found: %s", source)
		}
		if err := copyFile(source, cacheFile); err != nil {
			g.logger.Debug("failed to copy database to cache, using source directly",
				zap.String("source", source),
				zap.Error(err))
			// Update struct field when using source directly
			if version == xdb.IPv4 {
				g.localIPv4File = source
			} else {
				g.localIPv6File = source
			}
			cacheFile = source
		}
	}

	s, err := xdb.NewWithFileOnly(version, cacheFile)
	if err != nil {
		return fmt.Errorf("load database: %w", err)
	}
	g.lock.Lock()
	oldSearcher := *searcher
	*searcher = s
	g.lock.Unlock()
	if oldSearcher != nil {
		oldSearcher.Close()
	}

	g.logger.Info("loaded database",
		zap.String("source", source),
		zap.String("cache", cacheFile))
	return nil
}

func (g *GeoCity) updateDatabase(source, localFile string, version *xdb.Version, searcher **xdb.Searcher, label string) error {
	if !isHTTPSource(source) {
		return g.loadDatabase(source, localFile, version, searcher)
	}

	tempFile := localFile + ".temp"
	ctx, cancel := getContextWithTimeout(g.ctx, g.Timeout)
	defer cancel()

	if err := downloadFile(ctx, g.httpClient, source, tempFile); err != nil {
		if rmErr := os.Remove(tempFile); rmErr != nil {
			g.logger.Debug("failed to remove temp file", zap.String("file", tempFile), zap.Error(rmErr))
		}
		return fmt.Errorf("download %s database failed: %v", label, err)
	}

	tempSearcher, err := xdb.NewWithFileOnly(version, tempFile)
	if err != nil {
		if rmErr := os.Remove(tempFile); rmErr != nil {
			g.logger.Debug("failed to remove temp file", zap.String("file", tempFile), zap.Error(rmErr))
		}
		return fmt.Errorf("invalid %s database file: %v", label, err)
	}
	tempSearcher.Close()

	if err := os.Rename(tempFile, localFile); err != nil {
		if rmErr := os.Remove(tempFile); rmErr != nil {
			g.logger.Debug("failed to remove temp file", zap.String("file", tempFile), zap.Error(rmErr))
		}
		return fmt.Errorf("replace %s database file failed: %v", label, err)
	}

	newSearcher, err := xdb.NewWithFileOnly(version, localFile)
	if err != nil {
		return fmt.Errorf("open new %s database file failed: %v", label, err)
	}

	g.lock.Lock()
	oldSearcher := *searcher
	*searcher = newSearcher
	g.lock.Unlock()

	if oldSearcher != nil {
		oldSearcher.Close()
	}

	g.logger.Info(label+" database updated successfully", zap.String("file", localFile))
	return nil
}

func (g *GeoCity) updateDatabaseIPv4() error {
	return g.updateDatabase(g.IPv4Source, g.localIPv4File, xdb.IPv4, &g.searcherIPv4, "IPv4")
}

func (g *GeoCity) updateDatabaseIPv6() error {
	return g.updateDatabase(g.IPv6Source, g.localIPv6File, xdb.IPv6, &g.searcherIPv6, "IPv6")
}

func (g *GeoCity) periodicUpdate() {
	if g.Interval == 0 {
		g.Interval = caddy.Duration(24 * time.Hour)
	}

	ticker := time.NewTicker(time.Duration(g.Interval))
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			g.tryUpdateSource(g.IPv4Source, g.localIPv4File, g.updateDatabaseIPv4, "IPv4")
			g.tryUpdateSource(g.IPv6Source, g.localIPv6File, g.updateDatabaseIPv6, "IPv6")
		case <-g.ctx.Done():
			return
		}
	}
}

func (g *GeoCity) tryUpdateSource(source, localFile string, updateFn func() error, label string) {
	if source == "" || !isHTTPSource(source) {
		return
	}

	ctx, cancel := getContextWithTimeout(g.ctx, g.Timeout)
	defer cancel()

	ok, err := checkRemoteUpdate(ctx, g.httpClient, source, localFile, time.Duration(g.Interval))
	if err != nil {
		g.logger.Warn("check "+label+" update failed", zap.Error(err))
		return
	}
	if ok {
		if err := updateFn(); err != nil {
			g.logger.Error("update "+label+" database failed", zap.Error(err))
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
			case "cache":
				if d.NextArg() && d.Val() == "off" {
					enableCache := false
					g.EnableCache = &enableCache
					continue
				}
				enableCache := true
				g.EnableCache = &enableCache
				for d.NextArg() {
					switch d.Val() {
					case "ttl":
						if d.NextArg() {
							val, err := caddy.ParseDuration(d.Val())
							if err != nil {
								return err
							}
							g.CacheTTL = caddy.Duration(val)
						}
					case "size":
						if d.NextArg() {
							var maxSize int
							if _, err := fmt.Sscanf(d.Val(), "%d", &maxSize); err != nil {
								return d.Errf("invalid cache size: %s", d.Val())
							}
							if maxSize > 0 {
								g.CacheMaxSize = maxSize
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

// matchRegion checks if the region string contains any of the configured keywords.
// It searches in the full region string (e.g., "中国|0|北京|北京市|联通").
func (g *GeoCity) matchRegion(region string) bool {
	// Merge all keywords: Regions + Provinces + Cities (for backward compatibility)
	keywords := make([]string, 0, len(g.Regions)+len(g.Provinces)+len(g.Cities))
	keywords = append(keywords, g.Regions...)
	keywords = append(keywords, g.Provinces...)
	keywords = append(keywords, g.Cities...)

	if len(keywords) == 0 {
		return true // No filter configured, match all Chinese IPs
	}

	for _, kw := range keywords {
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
	var ip net.IP
	var ipStr string

	// Use Caddy's ClientIPVarKey which respects trusted_proxies configuration
	if clientIP, ok := caddyhttp.GetVar(r.Context(), caddyhttp.ClientIPVarKey).(string); ok && clientIP != "" {
		ip = getIP(clientIP)
		ipStr = clientIP
	}

	// Fallback to RemoteAddr if ClientIPVarKey is not set
	if ip == nil {
		g.logger.Debug("ClientIPVarKey not set, using RemoteAddr")
		ip = getIP(r.RemoteAddr)
		ipStr = r.RemoteAddr
	}

	if ip == nil {
		return false
	}

	// Lookup region and get match result
	region, matched := g.lookupAndMatch(ip)

	// Set variables for use in Caddyfile (e.g., header directive)
	// Always set these variables so they can be used with {http.vars.geocity_ip}
	caddyhttp.SetVar(r.Context(), "geocity_ip", ip.String())
	caddyhttp.SetVar(r.Context(), "geocity_region", region)

	g.logger.Debug("geocity match result",
		zap.String("client_ip", ipStr),
		zap.String("region", region),
		zap.Bool("matched", matched))

	return matched
}

// lookupAndMatch returns the region string and match result for an IP
func (g *GeoCity) lookupAndMatch(ip net.IP) (string, bool) {
	if ip == nil || checkPrivateIP(ip) {
		return "", false
	}

	ipStr := ip.String()

	// Check cache first
	if g.cache != nil {
		if result, found := g.cache.Get(ipStr); found {
			return result.Region, result.Matched
		}
	}

	// Use singleflight to deduplicate concurrent requests for the same IP
	result, _, _ := g.sfGroup.Do(ipStr, func() (any, error) {
		// Double-check cache after acquiring singleflight
		if g.cache != nil {
			if cached, found := g.cache.Get(ipStr); found {
				return cached, nil
			}
		}

		g.lock.RLock()
		defer g.lock.RUnlock()

		var searcher *xdb.Searcher
		if ip.To4() != nil {
			searcher = g.searcherIPv4
			if searcher == nil {
				return cityResult{}, nil
			}
		} else {
			searcher = g.searcherIPv6
			if searcher == nil {
				return cityResult{}, nil
			}
		}

		region, err := searcher.SearchByStr(ipStr)
		if err != nil {
			g.logger.Debug("failed to search IP location", zap.String("ip", ipStr), zap.Error(err))
			return cityResult{}, nil
		}

		// ip2region format: Country|Region|Province|City|ISP
		parts := strings.Split(region, "|")
		country := strings.TrimSpace(parts[0])

		var matched bool
		if country != "中国" {
			matched = false
		} else {
			matched = g.matchRegion(region)
		}

		res := cityResult{Region: region, Matched: matched}
		if g.cache != nil {
			g.cache.Set(ipStr, res)
		}

		return res, nil
	})

	if res, ok := result.(cityResult); ok {
		return res.Region, res.Matched
	}
	return "", false
}
