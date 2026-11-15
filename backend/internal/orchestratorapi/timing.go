package orchestratorapi

// Apply phase timings. A configuration change on this class of appliance takes
// tens of seconds, and "it is slow" is not actionable: the breakdown says which
// stage owns the time, both for support and for the next optimization.

import (
	"sync/atomic"
	"time"
)

// ApplyPhaseTiming is one stage of an apply.
type ApplyPhaseTiming struct {
	Phase      string `json:"phase"`
	DurationMS int64  `json:"duration_ms"`
}

// ApplyTiming is the timing of the most recent apply.
type ApplyTiming struct {
	RequestID  string            `json:"request_id"`
	Transaction string           `json:"transaction"`
	TotalMS    int64             `json:"total_ms"`
	Phases     []ApplyPhaseTiming `json:"phases"`
	Failed     bool              `json:"failed"`
	FinishedAt time.Time         `json:"finished_at"`
}

var lastApplyTiming atomic.Pointer[ApplyTiming]

// PublishApplyTiming records the timing of a finished apply.
func PublishApplyTiming(timing ApplyTiming) {
	if timing.FinishedAt.IsZero() {
		timing.FinishedAt = time.Now().UTC()
	}
	copy := timing
	lastApplyTiming.Store(&copy)
}

// LastApplyTiming returns the most recent apply timing, if any.
func LastApplyTiming() *ApplyTiming {
	return lastApplyTiming.Load()
}
