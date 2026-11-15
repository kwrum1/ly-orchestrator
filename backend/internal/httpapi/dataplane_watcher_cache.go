package httpapi

// The dataplane watcher ticks every few seconds. Rebuilding the interface
// inventory and the VPP interface list on every tick burned a measurable share
// of an Atom-class CPU (the gateway measured 73% of idle CPU on the same loop),
// so the inputs are cached and the one-shot repair is throttled.

import (
	"context"
	"sync"
	"time"
)

const (
	dataplaneWatcherInputTTL   = 15 * time.Second
	dataplaneWatcherMigrateTTL = time.Minute
)

type timedStringList struct {
	mu    sync.Mutex
	items []string
	at    time.Time
}

// timedNameSet caches a set-shaped input (the VPP interface names).
type timedNameSet struct {
	mu    sync.Mutex
	items map[string]bool
	at    time.Time
}

func (set *timedNameSet) current(build func() map[string]bool) map[string]bool {
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.items != nil && time.Since(set.at) < dataplaneWatcherInputTTL {
		return set.items
	}
	set.items = build()
	set.at = time.Now()
	return set.items
}

func (list *timedStringList) current(build func() []string) []string {
	list.mu.Lock()
	defer list.mu.Unlock()
	if list.items != nil && time.Since(list.at) < dataplaneWatcherInputTTL {
		return list.items
	}
	list.items = build()
	list.at = time.Now()
	return list.items
}

var (
	watchRuntimeInterfaces  timedStringList
	watchTakeoverInterfaces timedStringList
	watchVPPNames           timedNameSet

	watchMigrateMu sync.Mutex
	watchMigrateAt time.Time
	watchPruneMu   sync.Mutex
	watchPruneAt   time.Time
)

func (server *Server) cachedRuntimeDataInterfaces(ctx context.Context) []string {
	return watchRuntimeInterfaces.current(func() []string {
		return server.runtimeDataInterfaces(ctx)
	})
}

func (server *Server) cachedTakeoverDataInterfaces(ctx context.Context) []string {
	return watchTakeoverInterfaces.current(func() []string {
		return server.takeoverDataInterfaces(ctx)
	})
}

func (server *Server) cachedVPPInterfaceNames(ctx context.Context) map[string]bool {
	return watchVPPNames.current(func() map[string]bool {
		return server.vppInterfaceNames(ctx)
	})
}

// migrateForbiddenOnce runs the AF_XDP residue repair at most once a minute.
func (server *Server) migrateForbiddenOnce(ctx context.Context) {
	watchMigrateMu.Lock()
	due := watchMigrateAt.IsZero() || time.Since(watchMigrateAt) >= dataplaneWatcherMigrateTTL
	if due {
		watchMigrateAt = time.Now()
	}
	watchMigrateMu.Unlock()
	if !due {
		return
	}
	server.migrateForbiddenTakeoverModes(ctx)
}

// pruneStaleFactsOnce drops records for removed hardware at most once a minute.
func (server *Server) pruneStaleFactsOnce() {
	watchPruneMu.Lock()
	due := watchPruneAt.IsZero() || time.Since(watchPruneAt) >= dataplaneWatcherMigrateTTL
	if due {
		watchPruneAt = time.Now()
	}
	watchPruneMu.Unlock()
	if !due {
		return
	}
	server.pruneDataplaneFacts()
}
