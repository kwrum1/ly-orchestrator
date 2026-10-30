package service

import (
	"context"
	"sync"
	"time"
)

const (
	healthCacheTTL      = time.Second
	healthCheckTimeout  = 25 * time.Second
	healthUnknownTTL    = time.Second
	healthUnknownReason = "health probe timed out"
)

type runtimeHealthCache struct {
	mu      sync.Mutex
	entries map[ServiceName]runtimeHealthEntry
	flights map[ServiceName]*runtimeHealthFlight
}

type runtimeHealthEntry struct {
	health  Health
	expires time.Time
}

type runtimeHealthFlight struct {
	done   chan struct{}
	health Health
	err    error
}

func newRuntimeHealthCache() *runtimeHealthCache {
	return &runtimeHealthCache{
		entries: make(map[ServiceName]runtimeHealthEntry),
		flights: make(map[ServiceName]*runtimeHealthFlight),
	}
}

func (cache *runtimeHealthCache) invalidate() {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.entries = make(map[ServiceName]runtimeHealthEntry)
}

func (cache *runtimeHealthCache) check(ctx context.Context, service ServiceName, probe func(context.Context) (Health, error)) (Health, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now()
	cache.mu.Lock()
	if entry, ok := cache.entries[service]; ok && now.Before(entry.expires) {
		cache.mu.Unlock()
		return entry.health, nil
	}
	if flight, ok := cache.flights[service]; ok {
		// A caller with a short status budget must not turn a slow PID1 query
		// into a false component failure. If this service was healthy before,
		// return that last known state with an explicit stale reason while the
		// detached probe continues; only a service with no known state is
		// reported as unknown.
		if entry, cached := cache.entries[service]; cached && entry.health.Available {
			health := entry.health
			health.Reason = "health probe is slow; using last known running state"
			cache.mu.Unlock()
			select {
			case <-flight.done:
				return flight.health, flight.err
			case <-ctx.Done():
				return health, nil
			}
		}
		cache.mu.Unlock()
		select {
		case <-flight.done:
			return flight.health, flight.err
		case <-ctx.Done():
			return Health{Service: service, Available: false, TimedOut: true, Reason: healthUnknownReason}, nil
		}
	}
	flight := &runtimeHealthFlight{done: make(chan struct{})}
	cache.flights[service] = flight
	cache.mu.Unlock()

	// The probe is shared by every waiter and is deliberately detached from
	// the caller's request context.  Otherwise a status request that returns
	// (or whose handler context is cancelled) aborts the warm probe, caches a
	// false "unknown/degraded" result for the next request, and makes the
	// component state depend on request timing rather than systemd.
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), healthCheckTimeout)
	health, err := probe(probeCtx)
	cancel()
	if err == nil && !health.Available && health.Reason == "" {
		health.Reason = healthUnknownReason
	}
	if err != nil {
		health = Health{Service: service, Available: false, TimedOut: true, Reason: err.Error()}
	}
	if health.TimedOut {
		// Preserve a recent successful observation. A slow PID1 probe is not
		// evidence that an already-active Dnsmasq/Xray process failed.
		if previous, ok := cache.entries[service]; ok && previous.health.Available {
			health = previous.health
			health.Reason = "health probe timed out; using last known running state"
		}
	}

	cache.mu.Lock()
	if err == nil {
		ttl := healthCacheTTL
		if health.TimedOut {
			ttl = healthUnknownTTL
		}
		cache.entries[service] = runtimeHealthEntry{health: health, expires: time.Now().Add(ttl)}
	}
	flight.health = health
	flight.err = err
	delete(cache.flights, service)
	close(flight.done)
	cache.mu.Unlock()
	return health, err
}
