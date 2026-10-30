package vpp

import (
	"strings"
	"sync"
	"time"
)

const defaultVPPCTLCacheTTL = 3 * time.Second

type vppctlCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]vppctlCacheEntry
}

type vppctlCacheEntry struct {
	stdout  string
	stderr  string
	expires time.Time
}

func newVPPCTLCache(ttl time.Duration) *vppctlCache {
	if ttl <= 0 {
		ttl = defaultVPPCTLCacheTTL
	}
	return &vppctlCache{ttl: ttl, entries: make(map[string]vppctlCacheEntry)}
}

func (cache *vppctlCache) get(command string) (string, string, bool) {
	if cache == nil {
		return "", "", false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.entries[command]
	if !ok || time.Now().After(entry.expires) {
		if ok {
			delete(cache.entries, command)
		}
		return "", "", false
	}
	return entry.stdout, entry.stderr, true
}

func (cache *vppctlCache) put(command, stdout, stderr string) {
	if cache == nil {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.entries[command] = vppctlCacheEntry{stdout: stdout, stderr: stderr, expires: time.Now().Add(cache.ttl)}
}

func (cache *vppctlCache) invalidate() {
	if cache == nil {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.entries = make(map[string]vppctlCacheEntry)
}

func cacheableVPPCTLCommand(command string) bool {
	command = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(command), "?"))
	// Snapshot commands are read-only.  Never cache configuration or deletes:
	// the next readback must observe them immediately.
	return strings.HasPrefix(command, "show ") || command == "show"
}
