// Package selfheal records bounded repairs of product-owned dataplane state
// performed during one apply.
//
// A repair is evidence, not a silent success: the commit path persists the
// names recorded here (for example "nftables" or "abf") so an operator can see
// that the generation only converged because the runtime rebuilt stale state.
// The recorder travels on the apply context so the service layer (nftables) and
// the VPP layer (ABF) can contribute without adding a new return parameter to
// every call.
package selfheal

import (
	"context"
	"sort"
	"strings"
	"sync"
)

type recorderKey struct{}

// Recorder collects the distinct repair scopes of one apply.
type Recorder struct {
	mu    sync.Mutex
	names map[string]struct{}
}

// WithRecorder attaches a recorder to ctx and returns it for deferred reads.
func WithRecorder(ctx context.Context) (context.Context, *Recorder) {
	recorder := &Recorder{names: make(map[string]struct{}, 2)}
	return context.WithValue(ctx, recorderKey{}, recorder), recorder
}

// FromContext returns the recorder attached to ctx, or nil.
func FromContext(ctx context.Context) *Recorder {
	if ctx == nil {
		return nil
	}
	recorder, _ := ctx.Value(recorderKey{}).(*Recorder)
	return recorder
}

// Record marks one repair scope. It is a no-op when no recorder is attached, so
// direct service tests and non-transactional callers keep their behaviour.
func Record(ctx context.Context, name string) {
	recorder := FromContext(ctx)
	if recorder == nil {
		return
	}
	recorder.Add(name)
}

// Items returns a sorted copy of the repair scopes recorded on ctx.
func Items(ctx context.Context) []string {
	recorder := FromContext(ctx)
	if recorder == nil {
		return nil
	}
	return recorder.Items()
}

func (recorder *Recorder) Add(name string) {
	if recorder == nil {
		return
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	recorder.mu.Lock()
	recorder.names[name] = struct{}{}
	recorder.mu.Unlock()
}

func (recorder *Recorder) Items() []string {
	if recorder == nil {
		return nil
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	items := make([]string, 0, len(recorder.names))
	for name := range recorder.names {
		items = append(items, name)
	}
	sort.Strings(items)
	return items
}
