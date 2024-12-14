package geocn

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/oschwald/geoip2-golang"
	"go.uber.org/zap"
)

var (
	_ caddy.Module             = (*CNGeoIP)(nil)
	_ caddyhttp.RequestMatcher = (*CNGeoIP)(nil)
	_ caddy.Provisioner        = (*CNGeoIP)(nil)
	_ caddy.CleanerUpper       = (*CNGeoIP)(nil)
	_ caddyfile.Unmarshaler    = (*CNGeoIP)(nil)
)

const (
	remotefile = "https://github.com/Hackl0us/GeoIP2-CN/raw/release/Country.mmdb"
)

func init() {
	caddy.RegisterModule(CNGeoIP{})
}

type CNGeoIP struct {
	// refresh Interval
	Interval caddy.Duration `json:"interval,omitempty"`
	// request Timeout
	Timeout caddy.Duration `json:"timeout,omitempty"`
	// GeoIP2-CN remotefile
	RemoteFile string `json:"georemote,omitempty"`
	// GeoIP2-CN localfile
	GeoFile string `json:"geolocal,omitempty"`

	ctx      caddy.Context
	dbReader *geoip2.Reader
	logger   *zap.Logger
}

func (CNGeoIP) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.geocn",
		New: func() caddy.Module { return new(CNGeoIP) },
	}
}

// getContext returns a cancelable context, with a timeout if configured.
func (s *CNGeoIP) getContext() (context.Context, context.CancelFunc) {
	if s.Timeout > 0 {
		return context.WithTimeout(s.ctx, time.Duration(s.Timeout))
	}
	return context.WithCancel(s.ctx)
}

func (m *CNGeoIP) validSource(ip net.IP) bool {
	if ip == nil {
		return false
	}
	m.logger.Debug("valid ip", zap.String("ip", ip.String()))
	// 内网ip
	if !checkip(ip) {
		return false
	}
	record, err := m.dbReader.Country(ip)
	if err != nil || record == nil {
		return false
	}
	return record.Country.IsoCode == "CN"
}

func (m *CNGeoIP) Provision(ctx caddy.Context) error {
	m.ctx = ctx
	m.logger = ctx.Logger(m)

	// 检查 RemoteFile 是否为空
	if m.RemoteFile == "" {
		m.RemoteFile = remotefile
	}

	// 下载并加载文件
	if err := m.updateGeoFile(); err != nil {
		return err
	}

	// 如果设置了更新间隔，启动定时更新
	if m.Interval > 0 {
		go m.periodicUpdate()
	}

	return nil
}

func (m *CNGeoIP) updateGeoFile() error {
	// 下载文件
	if err := m.downloadFile(); err != nil {
		return fmt.Errorf("download file %v: %v", m.RemoteFile, err)
	}

	// 如果已存在 Reader，先关闭
	if m.dbReader != nil {
		m.dbReader.Close()
	}

	// 加载新文件
	var err error
	m.dbReader, err = geoip2.Open(m.GeoFile)
	if err != nil {
		return fmt.Errorf("open geodb file %v: %v", m.GeoFile, err)
	}

	m.logger.Debug("update geodb file", zap.String("geodb file", m.GeoFile), zap.String("remote file", m.RemoteFile))
	return nil
}

func (m *CNGeoIP) downloadFile() error {
	ctx, cancel := m.getContext()
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.RemoteFile, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download file %v: %v", m.RemoteFile, resp.StatusCode)
	}

	out, err := os.Create(m.GeoFile)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

func (m *CNGeoIP) periodicUpdate() {
	if m.Interval == 0 {
		m.Interval = caddy.Duration(time.Hour * 12)
	}
	ticker := time.NewTicker(time.Duration(m.Interval))
	defer ticker.Stop()

	for range ticker.C {
		if err := m.updateGeoFile(); err != nil {
			m.logger.Error("periodic update geodb file", zap.Error(err))
		}
	}
}

func (m *CNGeoIP) Cleanup() error {
	if m.dbReader != nil {
		return m.dbReader.Close()
	}
	return nil
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (m *CNGeoIP) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
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
			case "geolocal":
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.GeoFile = d.Val()
			case "georemote":
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.RemoteFile = d.Val()
			default:
				return d.ArgErr()
			}
		}
	}
	if len(m.GeoFile) == 0 {
		m.GeoFile = "/etc/caddy/Country.mmdb"
	}
	if len(m.RemoteFile) == 0 {
		m.RemoteFile = remotefile
	}
	return nil
}

func checkip(ip net.IP) bool {
	// 127.0.0.0/8
	// 224.0.0.0/4
	// 169.254.0.0/16
	// 10.0.0.0/8
	// 172.16.0.0/12
	// 192.168.0.0/16
	if ip.IsLoopback() || ip.IsLinkLocalMulticast() || ip.IsLinkLocalUnicast() || ip.IsPrivate() || ip.IsGlobalUnicast() {
		return false
	}
	return true
}

func (m *CNGeoIP) Match(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		m.logger.Warn("cannot split IP address", zap.String("address", r.RemoteAddr), zap.Error(err))
	}
	addr := net.ParseIP(host)
	// 中国公网ip
	if m.validSource(addr) {
		return true
	}
	if hVal := r.Header.Get("X-Forwarded-For"); hVal != "" {
		xhost := net.ParseIP(hVal)
		return m.validSource(xhost)
	}
	return false
}
