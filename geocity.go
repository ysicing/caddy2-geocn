package geocn

import (
	"context"
	"crypto/tls"
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
	ip2regionRemoteFile = "https://github.com/lionsoul2014/ip2region/raw/master/data/ip2region.xdb"
)

func init() {
	caddy.RegisterModule(GeoCity{})
}

type GeoCity struct {
	// 刷新间隔
	Interval caddy.Duration `json:"interval,omitempty"`
	// 请求超时
	Timeout caddy.Duration `json:"timeout,omitempty"`
	// ip2region 远程文件地址
	RemoteFile string `json:"remote_file,omitempty"`
	// ip2region 本地文件路径
	LocalFile string `json:"local_file,omitempty"`
	// 省份列表（根据mode决定是允许还是拒绝）
	Provinces []string `json:"provinces,omitempty"`
	// 城市列表（根据mode决定是允许还是拒绝）
	Cities []string `json:"cities,omitempty"`
	// 匹配模式：allow（白名单）或 deny（黑名单）
	Mode string `json:"mode,omitempty"`

	ctx      caddy.Context
	lock     *sync.RWMutex
	searcher *xdb.Searcher
	logger   *zap.Logger
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
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
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
	if g.RemoteFile == "" {
		g.RemoteFile = ip2regionRemoteFile
	}
	if g.LocalFile == "" {
		caddyDir := caddy.AppDataDir()
		g.LocalFile = filepath.Join(caddyDir, "geocity", "ip2region.xdb")
		if err := os.MkdirAll(filepath.Dir(g.LocalFile), 0755); err != nil {
			return fmt.Errorf("failed to create directory: %v", err)
		}
	}
	if g.Mode == "" {
		g.Mode = "allow" // 默认为白名单模式
	}
	if g.Timeout == 0 {
		g.Timeout = caddy.Duration(30 * time.Second)
	}

	// 尝试加载现有文件
	if searcher, err := xdb.NewWithFileOnly(g.LocalFile); err == nil {
		g.searcher = searcher
		g.logger.Debug("using existing ip2region database", zap.String("file", g.LocalFile))
	} else {
		// 文件不存在或无效，下载新文件
		if err := g.updateDatabase(); err != nil {
			return fmt.Errorf("initial database download failed: %v", err)
		}
	}

	// 启动定期更新
	go g.periodicUpdate()
	return nil
}

// 检查是否需要更新数据库
func (g *GeoCity) checkNeedUpdate() (bool, error) {
	// 如果文件不存在，需要更新
	if _, err := os.Stat(g.LocalFile); err != nil {
		return true, nil
	}

	// 检查远程文件是否可访问
	ctx, cancel := g.getContext()
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, g.RemoteFile, nil)
	if err != nil {
		return false, err
	}

	resp, err := g.getHTTPClient().Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK, nil
}

// 更新数据库文件
func (g *GeoCity) updateDatabase() error {
	tempFile := g.LocalFile + ".temp"

	// 下载到临时文件
	if err := g.downloadFile(tempFile); err != nil {
		os.Remove(tempFile)
		return fmt.Errorf("download failed: %v", err)
	}

	// 验证下载的文件
	tempSearcher, err := xdb.NewWithFileOnly(tempFile)
	if err != nil {
		os.Remove(tempFile)
		return fmt.Errorf("invalid database file: %v", err)
	}
	tempSearcher.Close()

	g.lock.Lock()
	defer g.lock.Unlock()

	// 如果存在旧的 Searcher，先关闭
	if g.searcher != nil {
		g.searcher.Close()
	}

	// 替换文件
	if err := os.Rename(tempFile, g.LocalFile); err != nil {
		os.Remove(tempFile)
		return fmt.Errorf("replace database file failed: %v", err)
	}

	// 加载新文件
	g.searcher, err = xdb.NewWithFileOnly(g.LocalFile)
	if err != nil {
		return fmt.Errorf("open new database file failed: %v", err)
	}

	g.logger.Info("ip2region database updated successfully", zap.String("file", g.LocalFile))
	return nil
}

func (g *GeoCity) downloadFile(file string) error {
	ctx, cancel := g.getContext()
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.RemoteFile, nil)
	if err != nil {
		return err
	}

	resp, err := g.getHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download file %v: %v", g.RemoteFile, resp.StatusCode)
	}

	out, err := os.Create(file)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
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
			if ok, _ := g.checkNeedUpdate(); ok {
				if err := g.updateDatabase(); err != nil {
					g.logger.Error("update database failed", zap.Error(err))
				}
			}
		case <-g.ctx.Done():
			return
		}
	}
}

func (g *GeoCity) Cleanup() error {
	if g.searcher != nil {
		g.searcher.Close()
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
			case "local_file":
				if !d.NextArg() {
					return d.ArgErr()
				}
				g.LocalFile = d.Val()
			case "remote_file":
				if !d.NextArg() {
					return d.ArgErr()
				}
				g.RemoteFile = d.Val()
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

	g.lock.RLock()
	defer g.lock.RUnlock()

	if g.searcher == nil {
		return false
	}

	// 查询IP地理位置信息
	region, err := g.searcher.SearchByStr(ip.String())
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
	if country != "中国" {
		switch g.Mode {
		case "allow":
			// 白名单模式：非中国IP默认不允许
			return false
		case "deny":
			// 黑名单模式：非中国IP默认允许
			return true
		default:
			return false
		}
	}

	g.logger.Debug("IP location info",
		zap.String("ip", ip.String()),
		zap.String("province", province),
		zap.String("city", city))

	switch g.Mode {
	case "allow":
		return g.isAllowed(province, city)
	case "deny":
		return !g.isDenied(province, city)
	default:
		return false
	}
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
