package dns

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestDNSCacheEvictsExpiredAndHonorsLimit(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	cache := NewDNSCacheWithLimit(func() time.Time { return now }, 2)
	outcome := CompiledOutcome{Kind: ResolverOutcomeDirect}

	cache.Store("expired.example", []string{"203.0.113.1"}, outcome, 10*time.Second)
	now = now.Add(11 * time.Second)
	cache.Store("fresh.example", []string{"203.0.113.2"}, outcome, 30*time.Second)
	if cache.Len() != 1 {
		t.Fatalf("cache length after expiry = %d, want only the fresh entry", cache.Len())
	}

	cache.Store("second.example", []string{"203.0.113.3"}, outcome, 20*time.Second)
	cache.Store("third.example", []string{"203.0.113.4"}, outcome, 30*time.Second)
	if cache.Len() != 2 {
		t.Fatalf("cache length = %d, want bounded to 2", cache.Len())
	}
	if _, ok := cache.Lookup("second.example"); ok {
		t.Fatal("earliest-expiring entry was not evicted")
	}
	if _, ok := cache.Lookup("fresh.example"); !ok {
		t.Fatal("longer-lived entry was evicted unexpectedly")
	}
	if _, ok := cache.Lookup("third.example"); !ok {
		t.Fatal("newest entry is missing")
	}
}

func TestDomainIPSetEvictsExpiredAndHonorsLimit(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	set := NewDomainIPSetWithLimit(func() time.Time { return now }, 2)
	outcome := CompiledOutcome{Kind: ResolverOutcomeDirect}

	set.Store("first.example", []string{"203.0.113.1"}, outcome, 10*time.Second)
	set.Store("second.example", []string{"203.0.113.2"}, outcome, 20*time.Second)
	set.Store("third.example", []string{"203.0.113.3"}, outcome, 30*time.Second)
	if set.Len() != 2 {
		t.Fatalf("ipset length = %d, want bounded to 2", set.Len())
	}
	if _, ok := set.Match("203.0.113.1"); ok {
		t.Fatal("earliest-expiring address was not evicted")
	}
	if snapshot := set.Snapshot(); len(snapshot.Entries) != 2 {
		t.Fatalf("snapshot length = %d, want 2", len(snapshot.Entries))
	}
}

func TestDNSCacheAndDomainIPSetConcurrentAccess(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	cache := NewDNSCacheWithLimit(func() time.Time { return now }, 32)
	set := NewDomainIPSetWithLimit(func() time.Time { return now }, 32)
	outcome := CompiledOutcome{Kind: ResolverOutcomeProxyResolution, ProxyEgressID: "proxy-a"}

	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for index := 0; index < 100; index++ {
				domain := fmt.Sprintf("w%d-%d.example", worker, index%7)
				ip := fmt.Sprintf("203.0.113.%d", index%20+1)
				cache.Store(domain, []string{ip}, outcome, time.Duration(index%5+1)*time.Second)
				cache.Lookup(domain)
				cache.Stale(domain)
				set.Store(domain, []string{ip}, outcome, time.Duration(index%5+1)*time.Second)
				set.Match(ip)
				set.Stale(ip)
				_ = set.Snapshot()
			}
		}(worker)
	}
	wait.Wait()
	if cache.Len() > 32 || set.Len() > 32 {
		t.Fatalf("cache sizes exceeded limits: cache=%d set=%d", cache.Len(), set.Len())
	}
}
