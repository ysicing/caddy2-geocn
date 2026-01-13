package geocn

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
)

// checkPrivateIP returns true if the IP is private, loopback, multicast, or unspecified.
func checkPrivateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsLinkLocalMulticast() || ip.IsLinkLocalUnicast() ||
		ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast()
}

// getHost extracts the host part from an address string (may be IP or host:port).
func getHost(s string) string {
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		host = s
	}
	return strings.TrimSpace(host)
}

// getIP parses an IP from a string that may include a port.
func getIP(s string) net.IP {
	return net.ParseIP(getHost(s))
}

// isHTTPSource returns true if the source is an HTTP/HTTPS URL.
func isHTTPSource(source string) bool {
	return strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
}

// copyFile copies a file from src to dst.
func copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destFile.Close()

	if _, err = io.Copy(destFile, sourceFile); err != nil {
		return err
	}
	return destFile.Sync()
}

// downloadFile downloads a file from remoteURL to localFile using the provided HTTP client.
func downloadFile(ctx context.Context, client *http.Client, remoteURL, localFile string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, remoteURL, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("downloading: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code %d for %s", resp.StatusCode, remoteURL)
	}

	out, err := os.Create(localFile)
	if err != nil {
		return fmt.Errorf("creating file: %w", err)
	}
	defer out.Close()

	if _, err = io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("writing file: %w", err)
	}
	return out.Sync()
}

// checkRemoteUpdate checks if a remote file needs to be updated based on Last-Modified header.
func checkRemoteUpdate(ctx context.Context, client *http.Client, remoteURL, localFile string, interval time.Duration) (bool, error) {
	if _, err := os.Stat(localFile); err != nil {
		return true, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, remoteURL, nil)
	if err != nil {
		return false, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("HEAD %s returned %s", remoteURL, resp.Status)
	}

	fi, statErr := os.Stat(localFile)
	lm := resp.Header.Get("Last-Modified")
	if lm == "" {
		if statErr != nil {
			return true, nil
		}
		if interval <= 0 {
			interval = 24 * time.Hour
		}
		return time.Since(fi.ModTime()) >= interval, nil
	}

	remoteTime, err := time.Parse(http.TimeFormat, lm)
	if err != nil {
		return false, fmt.Errorf("parse Last-Modified: %w", err)
	}
	if statErr != nil {
		return true, nil
	}
	return remoteTime.After(fi.ModTime()), nil
}

// newHTTPClient creates a new HTTP client with the given timeout.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			IdleConnTimeout:   timeout,
		},
	}
}

// getContextWithTimeout returns a context with timeout if timeout > 0, otherwise a cancelable context.
func getContextWithTimeout(ctx context.Context, timeout caddy.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, time.Duration(timeout))
	}
	return context.WithCancel(ctx)
}

// Generic cache implementation

// Cache is a generic TTL cache with LRU eviction.
type Cache[T any] struct {
	mu      sync.RWMutex
	entries map[string]*cacheEntry[T]
	maxSize int
	ttl     time.Duration
}

type cacheEntry[T any] struct {
	value     T
	timestamp time.Time
}

// NewCache creates a new cache with the given max size and TTL.
func NewCache[T any](maxSize int, ttl time.Duration) *Cache[T] {
	return &Cache[T]{
		entries: make(map[string]*cacheEntry[T]),
		maxSize: maxSize,
		ttl:     ttl,
	}
}

// Get retrieves a value from the cache. Returns the value and whether it was found.
func (c *Cache[T]) Get(key string) (T, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, exists := c.entries[key]
	if !exists || time.Since(entry.timestamp) > c.ttl {
		var zero T
		return zero, false
	}
	return entry.value, true
}

// Set stores a value in the cache.
func (c *Cache[T]) Set(key string, value T) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.entries[key]; !exists && len(c.entries) >= c.maxSize {
		c.evictOldest()
	}

	c.entries[key] = &cacheEntry[T]{
		value:     value,
		timestamp: time.Now(),
	}
}

func (c *Cache[T]) evictOldest() {
	var oldestKey string
	var oldestTime time.Time

	for key, entry := range c.entries {
		if oldestTime.IsZero() || entry.timestamp.Before(oldestTime) {
			oldestTime = entry.timestamp
			oldestKey = key
		}
	}

	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

// Cleanup removes expired entries periodically until context is done.
func (c *Cache[T]) Cleanup(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.mu.Lock()
			now := time.Now()
			for key, entry := range c.entries {
				if now.Sub(entry.timestamp) > c.ttl {
					delete(c.entries, key)
				}
			}
			c.mu.Unlock()
		case <-ctx.Done():
			return
		}
	}
}
