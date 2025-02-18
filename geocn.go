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
	"github.com/oschwald/geoip2-golang"
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
	// GeoIP2-CN remotefile
	RemoteFile string `json:"georemote,omitempty"`
	// GeoIP2-CN localfile
	GeoFile string `json:"geolocal,omitempty"`

	ctx      caddy.Context
	lock     *sync.RWMutex
	dbReader *geoip2.Reader
	logger   *zap.Logger
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
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
			DisableKeepAlives: true,
			IdleConnTimeout:   time.Duration(m.Timeout),
		},
	}
}

func (m *GeoCN) validSource(ip net.IP) bool {
	if ip == nil || checkPrivateIP(ip) {
		return false
	}

	m.lock.RLock()
	defer m.lock.RUnlock()

	record, err := m.dbReader.Country(ip)
	if err != nil || record == nil {
		return false
	}
	return record.Country.IsoCode == "CN"
}

func (m *GeoCN) Provision(ctx caddy.Context) error {
	m.ctx = ctx
	m.lock = new(sync.RWMutex)
	m.logger = ctx.Logger(m)

	// 设置默认值
	if m.RemoteFile == "" {
		m.RemoteFile = remotefile
	}
	if m.GeoFile == "" {
		// 使用 Caddy 的存储目录
		caddyDir := caddy.AppDataDir()
		m.GeoFile = filepath.Join(caddyDir, "geocn", "Country.mmdb")

		// 确保目录存在
		if err := os.MkdirAll(filepath.Dir(m.GeoFile), 0755); err != nil {
			return fmt.Errorf("failed to create directory: %v", err)
		}
	}

	// 尝试加载现有文件
	if reader, err := geoip2.Open(m.GeoFile); err == nil {
		m.dbReader = reader
		m.logger.Debug("using existing database", zap.String("file", m.GeoFile))
	} else {
		// 文件不存在或无效，下载新文件
		if err := m.updateGeoFile(); err != nil {
			return fmt.Errorf("initial database download failed: %v", err)
		}
	}

	go m.periodicUpdate()
	return nil
}

// 检查是否需要更新
func (m *GeoCN) checkNeedUpdate() (bool, error) {
	// 如果文件不存在，需要更新
	if _, err := os.Stat(m.GeoFile); err != nil {
		return true, nil
	}

	// 简单检查远程文件是否可访问
	ctx, cancel := m.getContext()
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, m.RemoteFile, nil)
	if err != nil {
		return false, err
	}

	resp, err := m.getHTTPClient().Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK, nil
}

// 更新文件
func (m *GeoCN) updateGeoFile() error {
	tempFile := m.GeoFile + ".temp"
	out, err := os.OpenFile(tempFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0777)
	if err != nil {
		return fmt.Errorf("create temporary file failed: %v", err)
	}
	defer out.Close()
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

func (m *GeoCN) downloadFile(file string) error {
	ctx, cancel := m.getContext()
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.RemoteFile, nil)
	if err != nil {
		return err
	}
	resp, err := m.getHTTPClient().Do(req)
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

func (m *GeoCN) periodicUpdate() {
	if m.Interval == 0 {
		m.Interval = caddy.Duration(time.Hour * 12)
	}

	ticker := time.NewTicker(time.Duration(m.Interval))
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if ok, _ := m.checkNeedUpdate(); ok {
				if err := m.updateGeoFile(); err != nil {
					m.logger.Error("update failed", zap.Error(err))
				}
			}
		case <-m.ctx.Done():
			return
		}
	}
}

func (m *GeoCN) Cleanup() error {
	if m.dbReader != nil {
		return m.dbReader.Close()
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

// MatchWithError implements caddyhttp.RequestMatcherWithError
func (m *GeoCN) MatchWithError(r *http.Request) (bool, error) {
	return m.Match(r), nil
}

func (m *GeoCN) Match(r *http.Request) bool {
	// 获取直接连接的 IP
	remoteIP := getIP(r.RemoteAddr)
	if remoteIP != nil && m.validSource(remoteIP) {
		return true
	}

	// 检查 X-Forwarded-For
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if clientIP := getIP(strings.Split(xff, ",")[0]); clientIP != nil {
			return m.validSource(clientIP)
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
