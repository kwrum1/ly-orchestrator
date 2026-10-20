package httpapi

import (
	"context"
	"sort"
	"sync"
)

// interfaceAliasCacheKey scopes host-interface enumeration to one runtime
// plan build.  A single plan resolves the same WAN/LAN IDs dozens of times;
// without this cache every lookup rescans /sys/class/net and netlink.
type interfaceAliasCacheKey struct{}

type interfaceAliasCache struct {
	once  sync.Once
	items []map[string]any
}

func withInterfaceAliasCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, interfaceAliasCacheKey{}, &interfaceAliasCache{})
}

func interfaceAliasCacheFromContext(ctx context.Context) *interfaceAliasCache {
	if ctx == nil {
		return nil
	}
	cache, _ := ctx.Value(interfaceAliasCacheKey{}).(*interfaceAliasCache)
	return cache
}

func (cache *interfaceAliasCache) snapshot() []map[string]any {
	cache.once.Do(func() {
		cache.items = hostInterfaceInventory()
		sort.SliceStable(cache.items, func(i, j int) bool {
			left := nonEmpty(stringField(cache.items[i], "id"), stringField(cache.items[i], "name"))
			right := nonEmpty(stringField(cache.items[j], "id"), stringField(cache.items[j], "name"))
			return left < right
		})
	})
	return cache.items
}
