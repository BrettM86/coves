package classification

import (
	"sync"
	"time"
)

// ProviderAuthPauseBase is how long the worker stops claiming after the
// provider first refuses its token (HTTP 401 or 403); each refusal that
// follows a pause doubles it, up to ProviderAuthPauseMax.
const ProviderAuthPauseBase = 5 * time.Minute

// ProviderAuthPauseMax is the longest provider auth pause.
const ProviderAuthPauseMax = time.Hour

// ProviderAuthPause is the worker's pause on a refused provider token. A
// refusal is the token's fault, not the job's, so it spends no attempt; the
// worker instead stops claiming until the pause ends. The pause is held in
// memory: a restarted AppView starts unpaused and probes the token with its
// first request. It is safe for concurrent use, and the metrics read it.
type ProviderAuthPause struct {
	mu    sync.Mutex
	until time.Time
	// next is the length of the next pause.
	next time.Duration
	// refusedModelID is the model whose refusal started the latest pause;
	// only a request it accepts resets the pause length.
	refusedModelID string
}

// NewProviderAuthPause returns an unpaused ProviderAuthPause.
func NewProviderAuthPause() *ProviderAuthPause {
	return &ProviderAuthPause{next: ProviderAuthPauseBase}
}

// PausedUntil reports when the pause ends, and whether it is still in force
// at now.
func (p *ProviderAuthPause) PausedUntil(now time.Time) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.until, now.Before(p.until)
}

// start pauses from now for the next pause length and doubles the one after,
// reporting the pause's end and length. modelID is the model that refused. A
// refusal while the pause is already in force, such as a concurrent request
// of the run that started it, leaves it unchanged and reports started false.
func (p *ProviderAuthPause) start(now time.Time, modelID string) (until time.Time, length time.Duration, started bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now.Before(p.until) {
		return p.until, 0, false
	}
	length = p.next
	p.until = now.Add(length)
	p.next = min(length*2, ProviderAuthPauseMax)
	p.refusedModelID = modelID
	return p.until, length, true
}

// reset makes the next pause ProviderAuthPauseBase again, after a request the
// provider accepted for modelID. A request accepted for another model leaves
// the pause length unchanged: when only one model refuses the token, such as
// a per-model permission, the pauses keep doubling toward
// ProviderAuthPauseMax.
func (p *ProviderAuthPause) reset(modelID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if modelID != p.refusedModelID {
		return
	}
	p.next = ProviderAuthPauseBase
}

// WithProviderAuthPause gives the worker the pause it shares with the metrics.
// A worker without one keeps its own.
func WithProviderAuthPause(pause *ProviderAuthPause) WorkerOption {
	return func(w *Worker) { w.authPause = pause }
}
