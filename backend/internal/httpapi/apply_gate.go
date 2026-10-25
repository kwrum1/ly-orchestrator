package httpapi

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// applyShutdownGraceDefault is how long a timed-out apply may take to
	// unwind after its context is cancelled. Child commands are already
	// process-group bounded, so this only covers Go-side cleanup. Once it
	// elapses the apply lock is released even if the worker is wedged in an
	// uninterruptible call; a wedged apply must never make the device
	// unconfigurable.
	applyShutdownGraceDefault = 3 * time.Second

	// proxyReconcileTimeout bounds one periodic proxy health reconciliation.
	// The reconciler is best effort: the next tick retries, so a slow cycle is
	// abandoned rather than allowed to pin the apply gate.
	proxyReconcileTimeout = 2 * time.Minute
)

// applyLease is one acquisition of the global apply gate. It is the only way
// production code acquires runtimeApplyMu; release is idempotent so a deferred
// release and a timeout path can both call it safely.
type applyLease struct {
	server        *Server
	kind          string
	transactionID string
	startedAt     time.Time
	deadline      time.Time
	expired       atomic.Bool
	once          sync.Once
}

// ApplyInProgressState is the read-only view of the current apply gate. It is
// exposed on /api/v1/runtime/status so operators can see which transaction
// owns the gate and when its bound expires instead of guessing from a stream
// of 202 responses.
type ApplyInProgressState struct {
	InProgress    bool      `json:"in_progress"`
	Kind          string    `json:"kind,omitempty"`
	TransactionID string    `json:"transaction_id,omitempty"`
	StartedAt     time.Time `json:"started_at,omitempty"`
	Deadline      time.Time `json:"deadline,omitempty"`
	Expired       bool      `json:"expired,omitempty"`
}

func (server *Server) applyTimeoutOrDefault() time.Duration {
	if server != nil && server.applyTimeout > 0 {
		return server.applyTimeout
	}
	return runtimeApplyTimeout
}

func (server *Server) applyGraceOrDefault() time.Duration {
	if server != nil && server.applyGrace > 0 {
		return server.applyGrace
	}
	return applyShutdownGraceDefault
}

// beginApply tries to take the global apply gate without queueing. Every apply
// path (operator runtime/config apply, async worker and proxy reconciler) uses
// this helper so the holder and its deadline are always visible.
func (server *Server) beginApply(kind, transactionID string, timeout time.Duration) (*applyLease, bool) {
	if !server.runtimeApplyMu.TryLock() {
		return nil, false
	}
	if timeout <= 0 {
		timeout = server.applyTimeoutOrDefault()
	}
	now := time.Now()
	lease := &applyLease{
		server:        server,
		kind:          kind,
		transactionID: transactionID,
		startedAt:     now.UTC(),
		deadline:      now.Add(timeout),
	}
	server.applyMu.Lock()
	server.applyHolder = lease
	server.applyMu.Unlock()
	return lease, true
}

// release clears the visible holder and unlocks the gate. It is safe to call
// more than once and from the timeout path.
func (lease *applyLease) release() {
	if lease == nil {
		return
	}
	lease.once.Do(func() {
		server := lease.server
		server.applyMu.Lock()
		if server.applyHolder == lease {
			server.applyHolder = nil
		}
		server.applyMu.Unlock()
		server.runtimeApplyMu.Unlock()
	})
}

func (server *Server) applyState() ApplyInProgressState {
	server.applyMu.Lock()
	holder := server.applyHolder
	server.applyMu.Unlock()
	if holder == nil {
		return ApplyInProgressState{InProgress: false}
	}
	state := ApplyInProgressState{
		InProgress:    true,
		Kind:          holder.kind,
		TransactionID: holder.transactionID,
		StartedAt:     holder.startedAt,
		Deadline:      holder.deadline,
	}
	if !holder.deadline.IsZero() {
		state.Expired = time.Now().After(holder.deadline)
	}
	return state
}

// publishRuntimeResult refuses to publish a late success after the transaction
// bound has already been reported as failed. The dataplane commit is still
// recorded by the executor; only the in-memory last-apply view stays failed.
func (server *Server) publishRuntimeResult(lease *applyLease, result RuntimeApplyResult) {
	if lease != nil && lease.expired.Load() {
		return
	}
	server.setRuntimeEvidence(result)
}

// bufferedHTTPResponse isolates an apply worker's response from the HTTP
// client. The handler goroutine owns it; the request goroutine only copies it
// after the worker has completed, so a timed-out worker can be abandoned
// without racing a response that will never be delivered.
type bufferedHTTPResponse struct {
	header     http.Header
	statusCode int
	body       bytes.Buffer
}

func newBufferedHTTPResponse() *bufferedHTTPResponse {
	return &bufferedHTTPResponse{header: make(http.Header)}
}

func (response *bufferedHTTPResponse) Header() http.Header {
	return response.header
}

func (response *bufferedHTTPResponse) WriteHeader(statusCode int) {
	if response.statusCode == 0 {
		response.statusCode = statusCode
	}
}

func (response *bufferedHTTPResponse) Write(payload []byte) (int, error) {
	if response.statusCode == 0 {
		response.statusCode = http.StatusOK
	}
	return response.body.Write(payload)
}

func (response *bufferedHTTPResponse) flushTo(w http.ResponseWriter) {
	for key, values := range response.header {
		w.Header()[key] = append([]string(nil), values...)
	}
	statusCode := response.statusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	w.WriteHeader(statusCode)
	_, _ = w.Write(response.body.Bytes())
}

// runBoundedWork runs work in its own goroutine and gives up after ctx expires
// plus a short grace period. The caller keeps ownership of the apply gate, so
// a worker stuck in an uninterruptible call cannot keep the gate locked.
func runBoundedWork(ctx context.Context, grace time.Duration, work func()) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("apply worker panic: %v", recovered)
			}
			close(done)
		}()
		work()
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		if grace < 0 {
			grace = 0
		}
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-done:
			return true
		case <-timer.C:
			return false
		}
	}
}

func applyTimeoutReason(kind string, timeout time.Duration) string {
	if kind == "" {
		kind = "apply"
	}
	if timeout <= 0 {
		timeout = runtimeApplyTimeout
	}
	return kind + " exceeded its " + timeout.String() + " bound"
}
