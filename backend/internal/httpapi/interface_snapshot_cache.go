package httpapi

// The interface list is polled by the console (and by the two extras queries the
// console runs on the same page), and one snapshot costs a VPP statistics read.
// A one-second single-flight cache collapses those duplicate calls without
// making the page stale: the UI's own refresh interval is 2-15 seconds.

import (
	"context"
	"net/http"
	"sync"
	"time"

	controlapi "ly-route/backend/internal/api"
)

const interfaceSnapshotTTL = time.Second

var interfaceSnapshotCache struct {
	mu    sync.Mutex
	at    time.Time
	items []map[string]any
	caps  []controlapi.CapabilityState
}

// cachedInterfaceRuntimeSnapshot returns the snapshot built within the last
// second, or builds a new one.
func (server *Server) cachedInterfaceRuntimeSnapshot(ctx context.Context) ([]map[string]any, []controlapi.CapabilityState, error) {
	interfaceSnapshotCache.mu.Lock()
	if interfaceSnapshotCache.items != nil && time.Since(interfaceSnapshotCache.at) < interfaceSnapshotTTL {
		items, caps := interfaceSnapshotCache.items, interfaceSnapshotCache.caps
		interfaceSnapshotCache.mu.Unlock()
		return items, caps, nil
	}
	interfaceSnapshotCache.mu.Unlock()

	items, caps, err := server.interfaceRuntimeSnapshot(ctx)
	if err != nil {
		return nil, nil, err
	}

	interfaceSnapshotCache.mu.Lock()
	interfaceSnapshotCache.at = time.Now()
	interfaceSnapshotCache.items = items
	interfaceSnapshotCache.caps = caps
	interfaceSnapshotCache.mu.Unlock()
	return items, caps, nil
}

// invalidateInterfaceSnapshotCache drops the cached snapshot after a mutation.
func invalidateInterfaceSnapshotCache() {
	interfaceSnapshotCache.mu.Lock()
	interfaceSnapshotCache.items = nil
	interfaceSnapshotCache.at = time.Time{}
	interfaceSnapshotCache.mu.Unlock()
}

// invalidateMutationMiddleware drops the interface snapshot before any state
// changing request, so a role edit or mirror change is visible on the very next
// read instead of up to one second later.
func invalidateMutationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			invalidateInterfaceSnapshotCache()
		}
		next.ServeHTTP(writer, request)
	})
}
