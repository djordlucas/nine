package runtime

import (
	"sync"
)

// JobWaiters lets job_wait block on a channel the sweeper closes when a job
// finishes, so N waiters on a job cost the sweeper one signal rather than N poll
// loops (docs/plugin-capabilities.md §5). It is created once at the daemon level
// and shared between the sweeper (which signals) and every worker's job tools
// (which wait). A nil *JobWaiters is never used directly; the job tools fall back
// to polling when none is wired (the eval harness).
type JobWaiters struct {
	mu      sync.Mutex
	waiters map[string][]chan struct{}
}

// NewJobWaiters returns an empty waiter registry.
func NewJobWaiters() *JobWaiters {
	log.Debug("creating JobWaiters")
	return &JobWaiters{waiters: make(map[string][]chan struct{})}
}

// register returns a channel closed when the handle is next signalled, plus a
// function that unregisters it (call on the waiter's exit so a timed-out or
// cancelled wait does not leak).
func (w *JobWaiters) register(handle string) (<-chan struct{}, func()) {
	log.Debug("JobWaiters.register", "handle", handle)
	ch := make(chan struct{})
	w.mu.Lock()
	w.waiters[handle] = append(w.waiters[handle], ch)
	w.mu.Unlock()
	log.Debug("JobWaiters.register added waiter", "handle", handle, "count", len(w.waiters[handle]))
	return ch, func() { w.remove(handle, ch) }
}

// signal wakes and clears every waiter on handle. The sweeper calls it once a job
// reaches a terminal state.
func (w *JobWaiters) signal(handle string) {
	log.Debug("JobWaiters.signal", "handle", handle, "waiter_count", len(w.waiters[handle]))
	w.mu.Lock()
	chs := w.waiters[handle]
	delete(w.waiters, handle)
	w.mu.Unlock()
	for _, ch := range chs {
		close(ch)
	}
	log.Debug("JobWaiters.signal closed channels", "handle", handle, "count", len(chs))
}

func (w *JobWaiters) remove(handle string, ch chan struct{}) {
	log.Debug("JobWaiters.remove", "handle", handle)
	w.mu.Lock()
	defer w.mu.Unlock()
	chs := w.waiters[handle]
	for i, c := range chs {
		if c == ch {
			w.waiters[handle] = append(chs[:i], chs[i+1:]...)
			log.Debug("JobWaiters.remove removed waiter", "handle", handle)
			break
		}
	}
	if len(w.waiters[handle]) == 0 {
		delete(w.waiters, handle)
		log.Debug("JobWaiters.remove deleted empty handle", "handle", handle)
	}
}
