package llm

import (
	"container/heap"
	"context"
	"sync"
)

type queueResult struct {
	resp Response
	err  error
}

type queueItem struct {
	ctx      context.Context
	req      Request
	priority int
	resultCh chan queueResult
	index    int // position in the heap; maintained by heap.Interface
}

// itemHeap is a min-heap ordered by priority (lower value = higher priority).
type itemHeap []*queueItem

func (h itemHeap) Len() int           { return len(h) }
func (h itemHeap) Less(i, j int) bool { return h[i].priority < h[j].priority }
func (h itemHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}
func (h *itemHeap) Push(x any) {
	item := x.(*queueItem)
	item.index = len(*h)
	*h = append(*h, item)
}
func (h *itemHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return item
}

// Queue is a prioritized LLM request queue that limits concurrent provider
// calls to maxConcurrent. Higher-priority requests (lower integer) jump ahead
// of waiting lower-priority ones when a slot opens.
type Queue struct {
	provider      Provider
	maxConcurrent int
	mu            sync.Mutex
	pending       itemHeap
	inflight      int
}

// NewQueue creates a Queue backed by provider, limiting concurrency.
func NewQueue(provider Provider, maxConcurrent int) *Queue {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &Queue{provider: provider, maxConcurrent: maxConcurrent}
}

// Submit enqueues a request at the given priority and blocks until the provider
// returns or ctx is cancelled. Lower priority values run first.
//
// A request that has to wait for a slot gets req.OnQueued when it is parked and
// req.OnDequeued when it starts; one that runs immediately gets neither. Both
// fire on the queue's goroutines, so callers must treat them as concurrent.
func (q *Queue) Submit(ctx context.Context, priority int, req Request) (Response, error) {
	item := &queueItem{
		ctx:      ctx,
		req:      req,
		priority: priority,
		resultCh: make(chan queueResult, 1),
	}

	q.mu.Lock()
	if q.inflight < q.maxConcurrent {
		q.inflight++
		q.mu.Unlock()
		go q.run(item)
	} else {
		heap.Push(&q.pending, item)
		q.mu.Unlock()
		if req.OnQueued != nil {
			req.OnQueued()
		}
	}

	select {
	case res := <-item.resultCh:
		return res.resp, res.err
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
}

// run executes item and then dispatches the next pending item if any.
func (q *Queue) run(item *queueItem) {
	var resp Response
	var err error
	if item.ctx.Err() != nil {
		err = item.ctx.Err()
	} else {
		resp, err = q.provider.Complete(item.ctx, item.req)
	}

	// Non-blocking send: if the caller already left via ctx.Done() the channel
	// is buffered(1) so this never blocks.
	select {
	case item.resultCh <- queueResult{resp, err}:
	default:
	}

	q.mu.Lock()
	q.inflight--
	if len(q.pending) > 0 {
		next := heap.Pop(&q.pending).(*queueItem)
		q.inflight++
		q.mu.Unlock()
		// Submit leaves cancelled items in the heap, so skip the callback when
		// the caller has already gone: it would report a queue wait against
		// whatever turn is running now.
		if next.req.OnDequeued != nil && next.ctx.Err() == nil {
			next.req.OnDequeued()
		}
		go q.run(next)
	} else {
		q.mu.Unlock()
	}
}

// SupportsThinking reports whether the underlying provider model supports
// native extended thinking. Returns false if the provider does not implement ThinkingAware.
// ThinkingAware implementations cache their probe, so this is cheap to call per turn.
func (q *Queue) SupportsThinking(ctx context.Context) bool {
	if ta, ok := q.provider.(ThinkingAware); ok {
		return ta.SupportsThinking(ctx)
	}
	return false
}

// Depth reports the queue's current load: pending is the number of requests
// waiting for a slot, inflight the number executing now, and maxConcurrent the
// slot limit. Surfaced in daemon status so operators can see LLM back-pressure.
func (q *Queue) Depth() (pending, inflight, maxConcurrent int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending), q.inflight, q.maxConcurrent
}

// ThinkingUnsupported reports whether the provider is capability-aware AND its
// model does not support native thinking — the exact trigger for the analysis-
// pass fallback. Providers that don't implement ThinkingAware return false
// (unknown ≠ unsupported), so they never get the fallback. This differs from
// !SupportsThinking, which is true for unknown providers too.
func (q *Queue) ThinkingUnsupported(ctx context.Context) bool {
	ta, ok := q.provider.(ThinkingAware)
	return ok && !ta.SupportsThinking(ctx)
}
