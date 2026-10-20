package service

import (
	"context"
	"sync"
	"time"
)

const (
	healthCacheTTL      = 15 * time.Second
	healthCheckTimeout  = 8 * time.Second
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

	probeCtx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	health, err := probe(probeCtx)
	cancel()
	if err == nil && !health.Available && health.Reason == "" {
		health.Reason = healthUnknownReason
	}
	if err != nil {
		health = Health{Service: service, Available: false, TimedOut: true, Reason: err.Error()}
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
