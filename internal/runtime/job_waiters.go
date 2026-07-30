package runtime

import "sync"

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
	return &JobWaiters{waiters: make(map[string][]chan struct{})}
}

// register returns a channel closed when the handle is next signalled, plus a
// function that unregisters it (call on the waiter's exit so a timed-out or
// cancelled wait does not leak).
func (w *JobWaiters) register(handle string) (<-chan struct{}, func()) {
	ch := make(chan struct{})
	w.mu.Lock()
	w.waiters[handle] = append(w.waiters[handle], ch)
	w.mu.Unlock()
	return ch, func() { w.remove(handle, ch) }
}

// signal wakes and clears every waiter on handle. The sweeper calls it once a job
// reaches a terminal state.
func (w *JobWaiters) signal(handle string) {
	w.mu.Lock()
	chs := w.waiters[handle]
	delete(w.waiters, handle)
	w.mu.Unlock()
	for _, ch := range chs {
		close(ch)
	}
}

func (w *JobWaiters) remove(handle string, ch chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	chs := w.waiters[handle]
	for i, c := range chs {
		if c == ch {
			w.waiters[handle] = append(chs[:i], chs[i+1:]...)
			break
		}
	}
	if len(w.waiters[handle]) == 0 {
		delete(w.waiters, handle)
	}
}
