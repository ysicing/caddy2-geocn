package geocn

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// getHost extracts the host part from an address string (may be IP or host:port).
func getHost(s string) string {
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		host = s
	}
	return strings.TrimSpace(host)
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

// downloadLocks prevents concurrent downloads of the same file.
var (
	downloadLocks   = make(map[string]*sync.Mutex)
	downloadLocksMu sync.Mutex
)

// getDownloadLock returns a mutex for the given file path.
func getDownloadLock(path string) *sync.Mutex {
	downloadLocksMu.Lock()
	defer downloadLocksMu.Unlock()
	if downloadLocks[path] == nil {
		downloadLocks[path] = &sync.Mutex{}
	}
	return downloadLocks[path]
}

// downloadFile downloads a file from remoteURL to localFile using the provided HTTP client.
// It downloads to a unique temporary file first, then atomically moves it to the target location.
// Concurrent calls with the same localFile will be serialized, and if the file already exists
// after acquiring the lock, the download is skipped.
func downloadFile(ctx context.Context, client *http.Client, remoteURL, localFile string) error {
	// Serialize downloads for the same file
	lock := getDownloadLock(localFile)
	lock.Lock()
	defer lock.Unlock()

	// Check if file already exists (another goroutine may have downloaded it)
	if _, err := os.Stat(localFile); err == nil {
		return nil
	}

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

	// Create unique temporary file in the same directory to ensure atomic rename works
	dir := filepath.Dir(localFile)
	out, err := os.CreateTemp(dir, filepath.Base(localFile)+".download.*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tempFile := out.Name()

	var success bool
	var closed bool
	defer func() {
		if !success {
			if !closed {
				out.Close()
			}
			os.Remove(tempFile)
		}
	}()

	if _, err = io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("writing file: %w", err)
	}

	if err = out.Sync(); err != nil {
		return fmt.Errorf("syncing file: %w", err)
	}

	// Close before rename to release the file handle
	if err = out.Close(); err != nil {
		closed = true
		return fmt.Errorf("closing temp file: %w", err)
	}
	closed = true

	// Atomically move to target location
	if err = os.Rename(tempFile, localFile); err != nil {
		return fmt.Errorf("moving file: %w", err)
	}

	success = true
	return nil
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
		c.evictOne()
	}

	c.entries[key] = &cacheEntry[T]{
		value:     value,
		timestamp: time.Now(),
	}
}

// evictOne removes one random entry from the cache.
// For IP geo-lookup caches, precise LRU ordering is unnecessary;
// random eviction is O(1) and avoids the O(n) full-table scan.
func (c *Cache[T]) evictOne() {
	for key := range c.entries {
		delete(c.entries, key)
		return
	}
}

// Cleanup removes expired entries periodically until context is done.
// Uses two-phase cleanup to minimize lock contention.
func (c *Cache[T]) Cleanup(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Phase 1: Collect expired keys with read lock
			c.mu.RLock()
			now := time.Now()
			var keysToDelete []string
			for key, entry := range c.entries {
				if now.Sub(entry.timestamp) > c.ttl {
					keysToDelete = append(keysToDelete, key)
				}
			}
			c.mu.RUnlock()

			// Phase 2: Delete expired entries with write lock
			if len(keysToDelete) > 0 {
				c.mu.Lock()
				for _, key := range keysToDelete {
					// Re-check to avoid deleting entries updated between phases
					if entry, exists := c.entries[key]; exists && now.Sub(entry.timestamp) > c.ttl {
						delete(c.entries, key)
					}
				}
				c.mu.Unlock()
			}
		case <-ctx.Done():
			return
		}
	}
}

// checkPrivateAddr returns true if the address is private, loopback, multicast, or unspecified.
func checkPrivateAddr(addr netip.Addr) bool {
	return addr.IsLoopback() || addr.IsLinkLocalMulticast() || addr.IsLinkLocalUnicast() ||
		addr.IsPrivate() || addr.IsUnspecified() || addr.IsMulticast()
}

// extractClientIP extracts the client IP host and raw string from an HTTP request.
// It uses Caddy's ClientIPVarKey (which respects trusted_proxies) with RemoteAddr fallback.
func extractClientIP(r *http.Request) (host, raw string) {
	if clientIP, ok := caddyhttp.GetVar(r.Context(), caddyhttp.ClientIPVarKey).(string); ok && clientIP != "" {
		return getHost(clientIP), clientIP
	}
	return getHost(r.RemoteAddr), r.RemoteAddr
}

// parseCacheBlock parses the cache directive in a Caddyfile block.
// It handles "cache off" and "cache ttl <dur> size <n>" syntax.
func parseCacheBlock(d *caddyfile.Dispenser, enableCache **bool, cacheTTL *caddy.Duration, cacheMaxSize *int) error {
	args := d.RemainingArgs()
	if len(args) > 0 && args[0] == "off" {
		off := false
		*enableCache = &off
		return nil
	}
	on := true
	*enableCache = &on
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "ttl":
			i++
			if i >= len(args) {
				return d.Errf("missing value for cache ttl")
			}
			val, err := caddy.ParseDuration(args[i])
			if err != nil {
				return err
			}
			*cacheTTL = caddy.Duration(val)
		case "size":
			i++
			if i >= len(args) {
				return d.Errf("missing value for cache size")
			}
			var maxSize int
			if _, err := fmt.Sscanf(args[i], "%d", &maxSize); err != nil {
				return d.Errf("invalid cache size: %s", args[i])
			}
			if maxSize > 0 {
				*cacheMaxSize = maxSize
			}
		default:
			return d.Errf("unknown cache option: %s", args[i])
		}
	}
	return nil
}
