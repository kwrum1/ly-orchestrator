package httpapi

// Local hardening that a management-plane appliance needs and that the gateway
// product already learned the hard way: bound credential guessing, keep the
// health sample stable, and make the bootstrap administrator survive a broken
// environment file.

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"ly-route/backend/internal/persistence"
)

const (
	loginFailureWindow = 5 * time.Minute
	loginFailureLimit  = 5
	loginBlockDuration = 60 * time.Second
	loginKeyLimit      = 1024
)

type loginFailure struct {
	count        int
	windowStart  time.Time
	blockedUntil time.Time
}

func clientAddress(r *http.Request) string {
	remote := strings.TrimSpace(r.RemoteAddr)
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return remote
	}
	return host
}

// loginBlocked reports whether this caller must wait before trying again.
func (server *Server) loginBlocked(key string) (time.Duration, bool) {
	server.loginMu.Lock()
	defer server.loginMu.Unlock()
	entry, ok := server.loginFailures[key]
	if !ok {
		return 0, false
	}
	now := server.now()
	if entry.blockedUntil.After(now) {
		return entry.blockedUntil.Sub(now), true
	}
	if now.Sub(entry.windowStart) > loginFailureWindow {
		delete(server.loginFailures, key)
	}
	return 0, false
}

func (server *Server) recordLoginFailure(key string) {
	server.loginMu.Lock()
	defer server.loginMu.Unlock()
	now := server.now()
	if server.loginFailures == nil {
		server.loginFailures = make(map[string]loginFailure, 8)
	}
	entry := server.loginFailures[key]
	if entry.windowStart.IsZero() || now.Sub(entry.windowStart) > loginFailureWindow {
		entry = loginFailure{windowStart: now}
	}
	entry.count++
	if entry.count >= loginFailureLimit {
		entry.blockedUntil = now.Add(loginBlockDuration)
	}
	server.loginFailures[key] = entry
	if len(server.loginFailures) <= loginKeyLimit {
		return
	}
	for itemKey, item := range server.loginFailures {
		if !item.blockedUntil.After(now) && now.Sub(item.windowStart) > loginFailureWindow {
			delete(server.loginFailures, itemKey)
		}
	}
}

func (server *Server) clearLoginFailures(key string) {
	server.loginMu.Lock()
	delete(server.loginFailures, key)
	server.loginMu.Unlock()
}

// cachedSystemSummary samples /proc at most once per second so back-to-back
// polls do not report a bogus 0% CPU sample.
func (server *Server) cachedSystemSummary() map[string]any {
	server.systemMu.Lock()
	defer server.systemMu.Unlock()
	now := server.now()
	if server.systemCache != nil && now.Sub(server.systemCacheAt) < time.Second {
		return server.systemCache
	}
	summary, _ := readSystemSummary()
	server.systemCache = summary
	server.systemCacheAt = now
	return summary
}

// EnsureBootstrapAdmin persists the environment-provisioned administrator so a
// later edit to the environment file cannot lock the operator out: the stored
// hash keeps working when the env value is removed or mistyped.
func EnsureBootstrapAdmin(ctx context.Context, store *persistence.Store, username, password string) error {
	if store == nil || strings.TrimSpace(username) == "" || strings.TrimSpace(password) == "" {
		return nil
	}
	if _, err := store.AuthUser(ctx, username); err == nil {
		return nil
	}
	return store.SaveAuthUser(ctx, persistence.AuthUser{
		Username:     username,
		Role:         "admin",
		PasswordHash: hashPassword(username, password),
	})
}
