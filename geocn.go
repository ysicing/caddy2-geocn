package caddy2_geocn

import (
	"fmt"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/oschwald/geoip2-golang"
	"go.uber.org/zap"
	"net"
	"net/http"
)

var (
	_ caddy.Module             = (*CNGeoIP)(nil)
	_ caddyhttp.RequestMatcher = (*CNGeoIP)(nil)
	_ caddy.Provisioner        = (*CNGeoIP)(nil)
	_ caddy.CleanerUpper       = (*CNGeoIP)(nil)
	_ caddyfile.Unmarshaler    = (*CNGeoIP)(nil)
)

func init() {
	caddy.RegisterModule(CNGeoIP{})
}

type CNGeoIP struct {
	DBFile   string `json:"db_file"`
	dbReader *geoip2.Reader
	logger   *zap.Logger
}

func (CNGeoIP) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.geocn",
		New: func() caddy.Module { return new(CNGeoIP) },
	}
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
	var err error
	m.dbReader, err = geoip2.Open(m.DBFile)
	m.logger = ctx.Logger(m)
	m.logger.Debug("provision ", zap.String("geodb file", m.DBFile))
	if err != nil {
		return fmt.Errorf("cannot  open geodb file %v: %v", m.DBFile, err)
	}
	return nil
}

func (m *CNGeoIP) Cleanup() error {
	if m.dbReader != nil {
		return m.dbReader.Close()
	}
	return nil
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (m *CNGeoIP) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	crt := 0
	for d.Next() {
		for n := d.Nesting(); d.NextBlock(n); {
			switch d.Val() {
			case "db_file":
				crt = 1
			default:
				switch crt {
				case 1:
					m.DBFile = d.Val()
					crt = 0
				}
			}
		}
	}
	if len(m.DBFile) == 0 {
		m.DBFile = "/etc/caddy/Country.mmdb"
	}
	return nil
}

func checkip(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalMulticast() || ip.IsLinkLocalUnicast() {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		switch true {
		case ip4[0] == 10:
			return false
		case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
			return false
		case ip4[0] == 192 && ip4[1] == 168:
			return false
		default:
			return true
		}
	}
	return false
}

func (m *CNGeoIP) Match(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		m.logger.Warn("cannot split IP address", zap.String("address", r.RemoteAddr), zap.Error(err))
	}
	addr := net.ParseIP(host)
	// 中国公网ip
	if m.validSource(addr) {
		m.logger.Info("")
		return true
	}
	if hVal := r.Header.Get("X-Forwarded-For"); hVal != "" {
		xhost := net.ParseIP(hVal)
		return m.validSource(xhost)
	}
	return false
}
