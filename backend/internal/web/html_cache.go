//go:build embed

package web

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// HTMLCache manages the cached index.html with injected settings
type HTMLCache struct {
	mu              sync.RWMutex
	cachedHTML      []byte
	etag            string
	homeHTML        []byte // Homepage variant with footer-links injection
	homeETag        string
	baseHTMLHash    string // Hash of the original index.html (immutable after build)
	settingsVersion uint64 // Incremented when settings change
}

// CachedHTML represents the cache state
type CachedHTML struct {
	Content []byte
	ETag    string
}

// NewHTMLCache creates a new HTML cache instance
func NewHTMLCache() *HTMLCache {
	return &HTMLCache{}
}

// SetBaseHTML initializes the cache with the base HTML template
func (c *HTMLCache) SetBaseHTML(baseHTML []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	hash := sha256.Sum256(baseHTML)
	c.baseHTMLHash = hex.EncodeToString(hash[:8]) // First 8 bytes for brevity
}

// Invalidate marks the cache as stale
func (c *HTMLCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.settingsVersion++
	c.cachedHTML = nil
	c.etag = ""
	c.homeHTML = nil
	c.homeETag = ""
}

// Get returns the cached HTML or nil if cache is stale
func (c *HTMLCache) Get() *CachedHTML {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.cachedHTML == nil {
		return nil
	}
	return &CachedHTML{
		Content: c.cachedHTML,
		ETag:    c.etag,
	}
}

// GetHome returns the cached homepage variant or nil if cache is stale
func (c *HTMLCache) GetHome() *CachedHTML {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.homeHTML == nil {
		return nil
	}
	return &CachedHTML{
		Content: c.homeHTML,
		ETag:    c.homeETag,
	}
}

// Version returns the current settings version, captured before fetching
// settings so a concurrent invalidation can be detected at commit time.
func (c *HTMLCache) Version() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.settingsVersion
}

// SetIfCurrent updates the cache with new rendered HTML only if the caller's
// captured version still matches the current one. If versions differ, nothing
// is written and false is returned.
func (c *HTMLCache) SetIfCurrent(html, homeHTML, settingsJSON []byte, wantVersion uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.settingsVersion != wantVersion {
		return false
	}

	c.cachedHTML = html
	c.etag = c.generateETag(settingsJSON, "")
	c.homeHTML = homeHTML
	c.homeETag = c.generateETag(settingsJSON, "-fl")
	return true
}

// generateETag creates an ETag from base HTML hash + settings hash, with an
// optional variant suffix appended before the closing quote.
func (c *HTMLCache) generateETag(settingsJSON []byte, variant string) string {
	settingsHash := sha256.Sum256(settingsJSON)
	return `"` + c.baseHTMLHash + "-" + hex.EncodeToString(settingsHash[:8]) + variant + `"`
}
