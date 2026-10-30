package dns

import (
	"net/netip"
	"sort"
	"sync"
	"time"
)

type CacheClock func() time.Time

const defaultDNSCacheLimit = 4096

type CachedResult struct {
	Domain        string          `json:"domain"`
	IPs           []string        `json:"ips"`
	Outcome       CompiledOutcome `json:"outcome"`
	CachedAt      time.Time       `json:"cached_at"`
	TTL           time.Duration   `json:"ttl"`
	ExpiresAt     time.Time       `json:"expires_at"`
	Expired       bool            `json:"expired"`
	Stale         bool            `json:"stale"`
	ProxyEgressID string          `json:"proxy_egress_id,omitempty"`
}

// DNSCache is safe for concurrent use. It is not wired into the runtime yet;
// the mutex and bounded eviction make it safe to attach to the resolver and
// status goroutines without introducing a fatal concurrent-map abort.
type DNSCache struct {
	mu      sync.RWMutex
	clock   CacheClock
	entries map[string]CachedResult
	limit   int
}

type DomainIPSetEntry struct {
	Domain        string          `json:"domain"`
	IP            string          `json:"ip"`
	Outcome       CompiledOutcome `json:"outcome"`
	CachedAt      time.Time       `json:"cached_at"`
	TTL           time.Duration   `json:"ttl"`
	ExpiresAt     time.Time       `json:"expires_at"`
	Expired       bool            `json:"expired"`
	Stale         bool            `json:"stale"`
	ProxyEgressID string          `json:"proxy_egress_id,omitempty"`
}

type DomainIPSetSnapshot struct {
	Entries []DomainIPSetEntry `json:"entries"`
}

type DomainIPSet struct {
	mu      sync.RWMutex
	clock   CacheClock
	entries map[string]DomainIPSetEntry
	limit   int
}

func NewDNSCache(clock CacheClock) *DNSCache {
	return NewDNSCacheWithLimit(clock, defaultDNSCacheLimit)
}

func NewDNSCacheWithLimit(clock CacheClock, limit int) *DNSCache {
	if clock == nil {
		clock = time.Now
	}
	if limit <= 0 {
		limit = defaultDNSCacheLimit
	}
	return &DNSCache{clock: clock, entries: make(map[string]CachedResult), limit: limit}
}

func NewDomainIPSet(clock CacheClock) *DomainIPSet {
	return NewDomainIPSetWithLimit(clock, defaultDNSCacheLimit)
}

func NewDomainIPSetWithLimit(clock CacheClock, limit int) *DomainIPSet {
	if clock == nil {
		clock = time.Now
	}
	if limit <= 0 {
		limit = defaultDNSCacheLimit
	}
	return &DomainIPSet{clock: clock, entries: make(map[string]DomainIPSetEntry), limit: limit}
}

func (cache *DNSCache) Store(domain string, ips []string, outcome CompiledOutcome, ttl time.Duration) CachedResult {
	now := cache.clock()
	result := CachedResult{
		Domain:        domain,
		IPs:           append([]string(nil), ips...),
		Outcome:       outcome,
		CachedAt:      now,
		TTL:           ttl,
		ExpiresAt:     now.Add(ttl),
		ProxyEgressID: outcome.ProxyEgressID,
	}
	if ttl <= 0 {
		result.Expired = true
		result.Stale = true
	}
	cache.mu.Lock()
	cache.entries[domain] = result
	cache.evictLocked(now)
	cache.mu.Unlock()
	return result
}

func (cache *DNSCache) Lookup(domain string) (CachedResult, bool) {
	now := cache.clock()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	result, exists := cache.entries[domain]
	if !exists {
		return CachedResult{}, false
	}
	if cache.isExpired(result, now) {
		result.Expired = true
		result.Stale = true
		cache.entries[domain] = result
		return CachedResult{}, false
	}
	return cloneCachedResult(result), true
}

func (cache *DNSCache) Stale(domain string) (CachedResult, bool) {
	now := cache.clock()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	result, exists := cache.entries[domain]
	if !exists || !cache.isExpired(result, now) {
		return CachedResult{}, false
	}
	result.Expired = true
	result.Stale = true
	cache.entries[domain] = result
	return cloneCachedResult(result), true
}

func (cache *DNSCache) Len() int {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	return len(cache.entries)
}

func (cache *DNSCache) isExpired(result CachedResult, now time.Time) bool {
	return !now.Before(result.ExpiresAt)
}

func (cache *DNSCache) evictLocked(now time.Time) {
	for key, entry := range cache.entries {
		if !now.Before(entry.ExpiresAt) {
			delete(cache.entries, key)
		}
	}
	if cache.limit <= 0 || len(cache.entries) <= cache.limit {
		return
	}
	type keyedResult struct {
		key    string
		result CachedResult
	}
	ordered := make([]keyedResult, 0, len(cache.entries))
	for key, entry := range cache.entries {
		ordered = append(ordered, keyedResult{key: key, result: entry})
	}
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].result.ExpiresAt.Equal(ordered[right].result.ExpiresAt) {
			return ordered[left].key < ordered[right].key
		}
		return ordered[left].result.ExpiresAt.Before(ordered[right].result.ExpiresAt)
	})
	for index := 0; index < len(ordered)-cache.limit; index++ {
		delete(cache.entries, ordered[index].key)
	}
}

func cloneCachedResult(result CachedResult) CachedResult {
	result.IPs = append([]string(nil), result.IPs...)
	return result
}

func (set *DomainIPSet) StoreResult(result CachedResult) []DomainIPSetEntry {
	entries := make([]DomainIPSetEntry, 0, len(result.IPs))
	for _, ip := range sortedUniqueIPs(result.IPs) {
		entry := DomainIPSetEntry{
			Domain:        result.Domain,
			IP:            ip,
			Outcome:       result.Outcome,
			CachedAt:      result.CachedAt,
			TTL:           result.TTL,
			ExpiresAt:     result.ExpiresAt,
			Expired:       result.Expired,
			Stale:         result.Stale,
			ProxyEgressID: result.ProxyEgressID,
		}
		if result.TTL <= 0 {
			entry.Expired = true
			entry.Stale = true
		}
		entries = append(entries, entry)
	}
	now := set.clock()
	set.mu.Lock()
	for _, entry := range entries {
		set.entries[entry.IP] = entry
	}
	set.evictLocked(now)
	set.mu.Unlock()
	return entries
}

func (set *DomainIPSet) Store(domain string, ips []string, outcome CompiledOutcome, ttl time.Duration) []DomainIPSetEntry {
	now := set.clock()
	return set.StoreResult(CachedResult{
		Domain:        domain,
		IPs:           append([]string(nil), ips...),
		Outcome:       outcome,
		CachedAt:      now,
		TTL:           ttl,
		ExpiresAt:     now.Add(ttl),
		ProxyEgressID: outcome.ProxyEgressID,
	})
}

func (set *DomainIPSet) Match(ip string) (DomainIPSetEntry, bool) {
	now := set.clock()
	set.mu.Lock()
	defer set.mu.Unlock()
	entry, exists := set.entries[ip]
	if !exists {
		return DomainIPSetEntry{}, false
	}
	if set.isExpired(entry, now) {
		entry.Expired = true
		entry.Stale = true
		set.entries[ip] = entry
		return DomainIPSetEntry{}, false
	}
	return entry, true
}

func (set *DomainIPSet) Stale(ip string) (DomainIPSetEntry, bool) {
	now := set.clock()
	set.mu.Lock()
	defer set.mu.Unlock()
	entry, exists := set.entries[ip]
	if !exists || !set.isExpired(entry, now) {
		return DomainIPSetEntry{}, false
	}
	entry.Expired = true
	entry.Stale = true
	set.entries[ip] = entry
	return entry, true
}

func (set *DomainIPSet) Len() int {
	set.mu.RLock()
	defer set.mu.RUnlock()
	return len(set.entries)
}

func (set *DomainIPSet) Snapshot() DomainIPSetSnapshot {
	set.mu.RLock()
	defer set.mu.RUnlock()
	ips := make([]string, 0, len(set.entries))
	for key := range set.entries {
		ips = append(ips, key)
	}
	ips = sortedUniqueIPs(ips)

	entries := make([]DomainIPSetEntry, 0, len(ips))
	for _, ip := range ips {
		entries = append(entries, set.entries[ip])
	}
	return DomainIPSetSnapshot{Entries: entries}
}

func DomainIPSetFromSnapshot(snapshot DomainIPSetSnapshot, clock CacheClock) *DomainIPSet {
	set := NewDomainIPSet(clock)
	set.mu.Lock()
	for _, entry := range snapshot.Entries {
		set.entries[entry.IP] = entry
	}
	set.evictLocked(set.clock())
	set.mu.Unlock()
	return set
}

func (set *DomainIPSet) isExpired(entry DomainIPSetEntry, now time.Time) bool {
	return !now.Before(entry.ExpiresAt)
}

func (set *DomainIPSet) evictLocked(now time.Time) {
	for key, entry := range set.entries {
		if !now.Before(entry.ExpiresAt) {
			delete(set.entries, key)
		}
	}
	if set.limit <= 0 || len(set.entries) <= set.limit {
		return
	}
	type keyedEntry struct {
		key   string
		entry DomainIPSetEntry
	}
	ordered := make([]keyedEntry, 0, len(set.entries))
	for key, entry := range set.entries {
		ordered = append(ordered, keyedEntry{key: key, entry: entry})
	}
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].entry.ExpiresAt.Equal(ordered[right].entry.ExpiresAt) {
			return ordered[left].key < ordered[right].key
		}
		return ordered[left].entry.ExpiresAt.Before(ordered[right].entry.ExpiresAt)
	})
	for index := 0; index < len(ordered)-set.limit; index++ {
		delete(set.entries, ordered[index].key)
	}
}

func sortedUniqueIPs(ips []string) []string {
	seen := make(map[string]struct{})
	unique := make([]string, 0, len(ips))
	for _, ip := range ips {
		addr, err := netip.ParseAddr(ip)
		if err != nil {
			continue
		}
		canonical := addr.String()
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		unique = append(unique, canonical)
	}
	sort.Slice(unique, func(left, right int) bool {
		leftAddr := netip.MustParseAddr(unique[left])
		rightAddr := netip.MustParseAddr(unique[right])
		return leftAddr.Less(rightAddr)
	})
	return unique
}
