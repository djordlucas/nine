package agent

import (
	"slices"
	"sync"
)

// Promotion reasons, as the tool_promoted journal event records them.
const (
	PromoteSearch = "search" // returned by tool_search
	PromoteCall   = "call"   // called from the index and failed (call-to-load)
)

// DefaultPromotedBudget bounds a deferred loop's promoted schemas, in estimated
// tokens, when [context] promoted_budget is unset.
const DefaultPromotedBudget = 1500

// PromotionEvent is one change to a loop's promoted set.
type PromotionEvent struct {
	Name     string
	Reason   string // PromoteSearch or PromoteCall; empty for a demotion
	Promoted bool   // false: demoted
}

// Promotions is the set of deferred tools a loop has loaded
// (adr/deferred-tool-schemas.md §3.3). A promoted tool is advertised with its
// schema from the loop's next LLM call on, for as long as the loop lives —
// across turns, and across a process session's wakes. Fit demotes the least
// recently called once the set outgrows its budget, between turns only, so a
// tool the model just loaded cannot vanish before it calls it.
//
// tool_search promotes from inside a dispatch and the loop reads the set while
// building a request. Both run on the loop's goroutine today; the mutex keeps
// the set safe if a handler ever runs elsewhere.
type Promotions struct {
	mu       sync.Mutex
	order    []string       // promotion order: the order the builder advertises them
	lastUsed map[string]int // name → sequence number of its last promotion or call
	seq      int
	events   []PromotionEvent
}

// NewPromotions returns an empty promoted set.
func NewPromotions() *Promotions {
	return &Promotions{lastUsed: map[string]int{}}
}

// Promote adds names not already promoted, in order, and records why.
func (p *Promotions) Promote(names []string, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, n := range names {
		p.seq++
		if _, ok := p.lastUsed[n]; !ok {
			p.order = append(p.order, n)
			p.events = append(p.events, PromotionEvent{Name: n, Reason: reason, Promoted: true})
		}
		p.lastUsed[n] = p.seq
	}
}

// Called records a call, which keeps a promoted tool from being demoted first.
func (p *Promotions) Called(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.lastUsed[name]; ok {
		p.seq++
		p.lastUsed[name] = p.seq
	}
}

// Names returns the promoted tools in promotion order.
func (p *Promotions) Names() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.order)
}

// Fit drops promoted tools the loop no longer has, then demotes the least
// recently used until the rest cost no more than budget. cost returns a tool's
// schema tokens, and ok false for a tool no longer in the catalog.
func (p *Promotions) Fit(budget int, cost func(name string) (tokens int, ok bool)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := 0
	kept := p.order[:0:0]
	for _, n := range p.order {
		c, ok := cost(n)
		if !ok {
			delete(p.lastUsed, n)
			p.events = append(p.events, PromotionEvent{Name: n})
			continue
		}
		kept = append(kept, n)
		total += c
	}
	p.order = kept
	for total > budget && len(p.order) > 0 {
		lru := p.order[0]
		for _, n := range p.order[1:] {
			if p.lastUsed[n] < p.lastUsed[lru] {
				lru = n
			}
		}
		c, _ := cost(lru)
		total -= c
		p.order = slices.DeleteFunc(p.order, func(n string) bool { return n == lru })
		delete(p.lastUsed, lru)
		p.events = append(p.events, PromotionEvent{Name: lru})
	}
}

// Drain returns the changes since the last Drain, oldest first.
func (p *Promotions) Drain() []PromotionEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	ev := p.events
	p.events = nil
	return ev
}
