package transport

import (
	"sync"
	"time"
)

// sessionTracker counts live streamable sessions per identity (phase 12
// B0.5). The SDK closes idle sessions itself (SessionTimeout); the tracker
// mirrors that with the same idle limit so a key cannot hold more than
// limits.max_live_sessions sessions. Entries leave on DELETE, on a 404 for
// the id, or when idle longer than the timeout.
type sessionTracker struct {
	mu   sync.Mutex
	byID map[string]*trackedSession
	now  func() time.Time
}

type trackedSession struct {
	owner    string
	lastSeen time.Time
}

func newSessionTracker() *sessionTracker {
	return &sessionTracker{byID: map[string]*trackedSession{}, now: time.Now}
}

func (t *sessionTracker) add(id, owner string) {
	t.mu.Lock()
	t.byID[id] = &trackedSession{owner: owner, lastSeen: t.now()}
	t.mu.Unlock()
}

func (t *sessionTracker) touch(id, owner string) {
	t.mu.Lock()
	if s := t.byID[id]; s != nil && s.owner == owner {
		s.lastSeen = t.now()
	}
	t.mu.Unlock()
}

func (t *sessionTracker) remove(id string) {
	t.mu.Lock()
	delete(t.byID, id)
	t.mu.Unlock()
}

// expire drops sessions idle longer than idle (caller holds mu).
func (t *sessionTracker) expire(idle time.Duration) {
	if idle <= 0 {
		return
	}
	cut := t.now().Add(-idle)
	for id, s := range t.byID {
		if s.lastSeen.Before(cut) {
			delete(t.byID, id)
		}
	}
}

func (t *sessionTracker) count(owner string, idle time.Duration) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expire(idle)
	n := 0
	for _, s := range t.byID {
		if s.owner == owner {
			n++
		}
	}
	return n
}

func (t *sessionTracker) counts(idle time.Duration) map[string]int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expire(idle)
	out := map[string]int{}
	for _, s := range t.byID {
		out[s.owner]++
	}
	return out
}
