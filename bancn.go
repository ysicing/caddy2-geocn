package caddy2bancn

import (
	"net"
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/oschwald/geoip2-golang"
)

var (
	_ caddyhttp.MiddlewareHandler = (*BanCN)(nil)
	_ caddyfile.Unmarshaler       = (*BanCN)(nil)
)

func init() {
	caddy.RegisterModule(BanCN{})
	httpcaddyfile.RegisterHandlerDirective("chinaip", parseCaddyfileHandle)

}

type BanCN struct {
	Header string
	DBFile string
}

func (BanCN) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.chinaip",
		New: func() caddy.Module { return new(BanCN) },
	}
}

// parseCaddyfileHandle unmarshals tokens from h into a new Middleware.
func parseCaddyfileHandle(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m BanCN
	err := m.UnmarshalCaddyfile(h.Dispenser)
	if err != nil {
		return nil, err
	}
	return m, err
}

func parseStringArg(d *caddyfile.Dispenser, out *string) error {
	if !d.Args(out) {
		return d.ArgErr()
	}
	return nil
}

func (m *BanCN) validSource(addr string) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	db, err := geoip2.Open(m.DBFile)
	if err != nil {
		return false
	}
	defer db.Close()
	record, err := db.Country(ip)
	if err != nil || record == nil {
		return false
	}
	return record.Country.IsoCode == "CN"
}

// func (m *BanCN) Provision(ctx caddy.Context) error {
// 	switch m.Output {
// 	case "stdout":
// 		m.w = os.Stdout
// 	case "stderr":
// 		m.w = os.Stderr
// 	default:
// 		return fmt.Errorf("an output stream is required")
// 	}
// 	return nil
// }

// func (m *BanCN) Validate() error {
// 	if m.w == nil {
// 		return fmt.Errorf("no writer")
// 	}
// 	return nil
// }

// ServeHTTP implements caddyhttp.MiddlewareHandler.
func (m BanCN) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return next.ServeHTTP(w, r)
	}
	if hVal := r.Header.Get(m.Header); hVal != "" {

	}
	return next.ServeHTTP(w, r)
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (m *BanCN) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.NextArg()
	for d.NextBlock(0) {
		var err error
		switch d.Val() {
		case "header":
			err = parseStringArg(d, &m.Header)
		default:
			return d.Errf("unknown chinaip arg")
		}
		if err != nil {
			return d.Errf("error parsing %s: %s", d.Val(), err)
		}
	}
	return nil
}
