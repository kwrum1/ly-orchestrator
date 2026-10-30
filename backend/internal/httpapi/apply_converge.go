package httpapi

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// supersededApply marks an apply that aborted because the desired configuration
// changed while it was running. A UI save writes several documents (interface,
// WAN link, dependency graph), so an apply issued between those writes is
// legitimately stale. Refusing to commit it is correct; replaying the previous
// committed generation is not, because the newest generation would never reach
// the dataplane and the rollback could leave ports half released.
var supersededApply atomic.Bool

const (
	convergeApplyWindow = 2 * time.Minute
	convergeApplyMax    = 5
)

var (
	convergeApplyMu    sync.Mutex
	convergeApplyCount int
	convergeApplyFirst time.Time
)

// scheduleConvergeApply runs one more apply against the newest desired
// generation after a superseded apply. It is bounded so a client stuck in a
// write loop cannot spin the dataplane forever.
func (server *Server) scheduleConvergeApply() {
	now := time.Now()
	convergeApplyMu.Lock()
	if convergeApplyFirst.IsZero() || now.Sub(convergeApplyFirst) > convergeApplyWindow {
		convergeApplyFirst = now
		convergeApplyCount = 0
	}
	if convergeApplyCount >= convergeApplyMax {
		convergeApplyMu.Unlock()
		log.Printf("runtime apply: configuration kept changing during apply; giving up after %d converge retries", convergeApplyMax)
		return
	}
	convergeApplyCount++
	convergeApplyMu.Unlock()

	go func() {
		// Let the document write that superseded the plan finish first.
		time.Sleep(2 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), server.applyTimeoutOrDefault())
		defer cancel()
		transactionID := "runtime-" + newRequestID()
		lease, acquired := server.beginApply("runtime-apply", transactionID, 0)
		if !acquired {
			log.Printf("runtime apply: converge retry skipped; another apply owns the gate")
			return
		}
		defer lease.release()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/runtime/apply", nil)
		if err != nil {
			return
		}
		request.URL = &url.URL{Path: "/api/v1/runtime/apply"}
		request.RemoteAddr = "127.0.0.1:1"
		request.Header.Set("X-Request-Id", "converge-"+newRequestID())
		request.Header.Set("X-Ly-Route-Converge", "1")
		log.Printf("runtime apply: desired configuration changed mid-apply; converging on the newest generation (%s)", transactionID)
		buffered := newBufferedHTTPResponse()
		server.executeRuntimeApply(buffered, request, Session{Username: "system", Role: "admin"}, ctx, transactionID, lease)
	}()
}
