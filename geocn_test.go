package geocn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/lionsoul2014/ip2region/binding/golang/xdb"
	geoip2 "github.com/oschwald/geoip2-golang/v2"
	"go.uber.org/zap"
)

func fixturePath(t *testing.T, name string) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("unable to determine caller information")
	}
	return filepath.Join(filepath.Dir(filename), name)
}

func copyTestFile(t *testing.T, src, dst string) {
	t.Helper()
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copy file: %v", err)
	}
}

func newTestContext() caddy.Context {
	return caddy.Context{Context: context.Background()}
}

func TestGeoCNUpdateGeoFileReplacesReader(t *testing.T) {
	fixture := fixturePath(t, "Country.mmdb")
	if _, err := os.Stat(fixture); err != nil {
		t.Fatalf("fixture missing: %v", err)
	}

	tmpDir := t.TempDir()
	localFile := filepath.Join(tmpDir, "Country.mmdb")
	copyTestFile(t, fixture, localFile)

	initialReader, err := geoip2.Open(localFile)
	if err != nil {
		t.Fatalf("open initial reader: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.ServeFile(w, r, fixture)
	}))
	defer srv.Close()

	module := &GeoCN{
		Timeout:   caddy.Duration(time.Second),
		Source:    srv.URL,
		localFile: localFile,
		ctx:       newTestContext(),
		lock:      &sync.RWMutex{},
		dbReader:  initialReader,
		logger:    zap.NewNop(),
		httpClient: &http.Client{
			Timeout: time.Second,
			Transport: &http.Transport{
				DisableKeepAlives: true,
				IdleConnTimeout:   time.Second,
			},
		},
	}

	if err := module.updateGeoFile(); err != nil {
		t.Fatalf("updateGeoFile failed: %v", err)
	}

	if module.dbReader == nil {
		t.Fatal("expected dbReader to be set")
	}
	if module.dbReader == initialReader {
		t.Fatal("expected dbReader to be replaced")
	}

	if addr, err := netip.ParseAddr("1.1.1.1"); err != nil {
		t.Fatalf("parse ip: %v", err)
	} else if _, err := module.dbReader.Country(addr); err != nil {
		t.Fatalf("new reader lookup failed: %v", err)
	}

	var oldErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("old reader call panicked: %v", r)
			}
		}()
		if addr, err := netip.ParseAddr("1.1.1.1"); err != nil {
			oldErr = err
		} else {
			_, oldErr = initialReader.Country(addr)
		}
	}()
	if oldErr == nil {
		t.Fatalf("expected old reader to report an error after update")
	}

	t.Cleanup(func() {
		module.dbReader.Close()
	})
}

func TestGeoCityUpdateDatabaseReplacesSearcher(t *testing.T) {
	fixture := fixturePath(t, "ip2region.xdb")
	if _, err := os.Stat(fixture); err != nil {
		t.Fatalf("fixture missing: %v", err)
	}

	tmpDir := t.TempDir()
	localFile := filepath.Join(tmpDir, "ip2region.xdb")
	copyTestFile(t, fixture, localFile)

	initialSearcher, err := xdb.NewWithFileOnly(xdb.IPv4, localFile)
	if err != nil {
		t.Fatalf("open initial searcher: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.ServeFile(w, r, fixture)
	}))
	defer srv.Close()

	module := &GeoCity{
		Timeout:       caddy.Duration(time.Second),
		IPv4Source:    srv.URL,
		localIPv4File: localFile,
		ctx:           newTestContext(),
		lock:          &sync.RWMutex{},
		searcherIPv4:  initialSearcher,
		logger:        zap.NewNop(),
		httpClient: &http.Client{
			Timeout: time.Second,
			Transport: &http.Transport{
				DisableKeepAlives: true,
				IdleConnTimeout:   time.Second,
			},
		},
	}

	if err := module.updateDatabaseIPv4(); err != nil {
		t.Fatalf("updateDatabaseIPv4 failed: %v", err)
	}

	if module.searcherIPv4 == nil {
		t.Fatal("expected searcherIPv4 to be set")
	}
	if module.searcherIPv4 == initialSearcher {
		t.Fatal("expected searcherIPv4 to be replaced")
	}

	if _, err := module.searcherIPv4.SearchByStr("1.1.1.1"); err != nil {
		t.Fatalf("new searcher lookup failed: %v", err)
	}

	if _, err := initialSearcher.SearchByStr("1.1.1.1"); err == nil {
		t.Fatalf("expected old searcher to report an error after update")
	}

	t.Cleanup(func() {
		if module.searcherIPv4 != nil {
			module.searcherIPv4.Close()
		}
	})
}

func TestDownloadFileRejectsInvalidTLS(t *testing.T) {
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer tlsSrv.Close()

	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "download.tmp")

	client := &http.Client{
		Timeout: 500 * time.Millisecond,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			IdleConnTimeout:   500 * time.Millisecond,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := downloadFile(ctx, client, tlsSrv.URL, target)
	if err == nil {
		t.Fatalf("expected TLS download to fail due to self-signed certificate")
	}
}
