package geocn

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
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
	lock     *sync.RWMutex
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
		m.logger.Warn("valid ip", zap.String("ip", "nil"))
		return false
	}
	m.logger.Debug("valid ip", zap.String("ip", ip.String()))
	if checkPrivateIP(ip) {
		return false
	}

	m.lock.RLock()         // 添加读锁
	defer m.lock.RUnlock() // 确保锁会被释放

	record, err := m.dbReader.Country(ip)
	if err != nil {
		m.logger.Warn("valid ip", zap.String("ip", ip.String()), zap.Error(err))
		return false
	}
	if record == nil {
		return false
	}
	return record.Country.IsoCode == "CN"
}

func (m *CNGeoIP) Provision(ctx caddy.Context) error {
	m.ctx = ctx
	m.lock = new(sync.RWMutex)
	m.logger = ctx.Logger(m)

	// 检查 RemoteFile 是否为空
	if len(m.RemoteFile) == 0 {
		m.RemoteFile = remotefile
	}
	if len(m.GeoFile) == 0 {
		m.GeoFile = "/etc/caddy/Country.mmdb"
	}
	fileExists := false
	if fileInfo, err := os.Stat(m.GeoFile); err == nil {
		// 尝试打开现有文件
		reader, err := geoip2.Open(m.GeoFile)
		if err == nil {
			m.dbReader = reader
			fileExists = true
			m.logger.Debug("found existing database",
				zap.String("file", m.GeoFile),
				zap.Time("modified", fileInfo.ModTime()))
		}
	}
	if fileExists {
		// 文件存在，检查是否需要更新
		needUpdate, err := m.checkNeedUpdate()
		if err != nil {
			m.logger.Warn("failed to check updates", zap.Error(err))
			// 检查更新失败时继续使用现有文件
		} else if needUpdate {
			// 需要更新时尝试更新
			if err := m.updateGeoFile(); err != nil {
				m.logger.Warn("failed to update database", zap.Error(err))
				// 更新失败时继续使用现有文件
			}
		}
	} else {
		// 文件不存在，必须下载
		if err := m.updateGeoFile(); err != nil {
			return fmt.Errorf("initial database download failed: %v", err)
		}
	}

	go m.periodicUpdate()
	return nil
}

// 检查是否需要更新
func (m *CNGeoIP) checkNeedUpdate() (bool, error) {
	// 检查本地文件
	fileInfo, err := os.Stat(m.GeoFile)
	if os.IsNotExist(err) {
		return true, nil // 本地文件不存在，需要更新
	}
	if fileInfo.ModTime().Before(time.Now().Add(-time.Hour * 12)) {
		return true, nil // 本地文件修改时间超过12小时，需要更新
	}

	ctx, cancel := m.getContext()
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, m.RemoteFile, nil)
	if err != nil {
		return false, err
	}

	client := &http.Client{
		Timeout: time.Second * 10,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
			DisableKeepAlives: true,
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("remote file check failed: %d", resp.StatusCode)
	}

	return true, nil
}

// 更新文件
func (m *CNGeoIP) updateGeoFile() error {
	tempFile := m.GeoFile + ".temp"

	// 下载到临时文件
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

	// 如果存在旧的 Reader，先关闭
	if m.dbReader != nil {
		m.dbReader.Close()
	}

	// 替换文件
	if err := os.Rename(tempFile, m.GeoFile); err != nil {
		os.Remove(tempFile)
		return fmt.Errorf("replace database file failed: %v", err)
	}

	// 加载新文件
	m.dbReader, err = geoip2.Open(m.GeoFile)
	if err != nil {
		return fmt.Errorf("open new database file failed: %v", err)
	}
	return nil
}

func (m *CNGeoIP) downloadFile(file string) error {
	ctx, cancel := m.getContext()
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.RemoteFile, nil)
	if err != nil {
		return err
	}
	var client = &http.Client{
		Timeout: time.Second * 30,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
			DisableKeepAlives: true,
			IdleConnTimeout:   time.Second * 30,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download file %v: %v", m.RemoteFile, resp.StatusCode)
	}

	out, err := os.Create(file)
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
	for {
		select {
		case <-ticker.C:
			needUpdate, err := m.checkNeedUpdate()
			if err != nil {
				m.logger.Error("check update failed", zap.Error(err))
				continue
			}
			if needUpdate {
				if err := m.updateGeoFile(); err != nil {
					m.logger.Error("update geofile failed", zap.Error(err))
				}
			}
		case <-m.ctx.Done():
			return
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

func (m *CNGeoIP) Match(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		m.logger.Warn("cannot split IP address", zap.String("address", r.RemoteAddr), zap.Error(err))
		return false
	}
	addr := net.ParseIP(host)
	if m.validSource(addr) {
		return true
	}
	if hVal := r.Header.Get("X-Forwarded-For"); hVal != "" {
		ips := strings.Split(hVal, ",")
		if len(ips) > 0 {
			xhost := net.ParseIP(strings.TrimSpace(ips[0]))
			if m.validSource(xhost) {
				return true
			}
		}
	}
	return false
}
