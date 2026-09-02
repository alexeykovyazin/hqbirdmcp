//go:build windows

package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/getlantern/systray"

	"github.com/aleks/fbmcp/internal/state"
)

const (
	pollInterval = 3 * time.Second
	// escSnooze is how long a dialog dismissed without a button (Esc/close)
	// stays quiet before re-prompting (WS2.6e). Previously an Escaped action
	// was never shown again for its whole 15-minute TTL.
	escSnooze = 60 * time.Second
)

// tracker is the shared shown/snoozed bookkeeping between the poll loop and
// the dialog worker.
type tracker struct {
	mu     sync.Mutex
	shown  map[string]bool
	snooze map[string]time.Time
}

func newTracker() *tracker {
	return &tracker{shown: map[string]bool{}, snooze: map[string]time.Time{}}
}

// Snooze marks an id dismissed-without-answer: it becomes eligible for
// re-queueing after escSnooze.
func (t *tracker) Snooze(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.shown, id)
	t.snooze[id] = time.Now().Add(escSnooze)
}

// pollLoop re-opens the state store every tick — state.Open reads
// state.json once per call (it is not a live handle), so this is the only
// way a second process notices pending actions the running fbmcp server
// added after this process started. New Tier >= 2 ids are queued once each;
// ids leave the shown/snooze sets when they leave Pending() (consumed by
// approval/denial/expiry). Ids are never reused (gate.newID is 12 random
// bytes), so pruning is only about bounding memory.
func pollLoop(stateDir string, tr *tracker, queue chan<- state.PendingAction) {
	for {
		pollOnce(stateDir, tr, queue)
		time.Sleep(pollInterval)
	}
}

// pollOnce is one poll cycle behind a recover: a panic on a worker
// goroutine is process-fatal and invisible in a windowsgui binary, so a
// bad tick must degrade to a skipped cycle, not a silent tray death.
// (The dialog path recovers per-dialog in showOneDialog.)
func pollOnce(stateDir string, tr *tracker, queue chan<- state.PendingAction) {
	defer func() {
		if r := recover(); r != nil {
			logErr("poll tick panic (recovered): %v", r)
		}
	}()
	st, err := state.Open(stateDir)
	if err != nil {
		// WS2.6c: previously swallowed — pending approvals would just
		// stop appearing with no operator-visible signal.
		systray.SetTooltip("fbmcp — state unreadable: " + err.Error())
		return // pollLoop sleeps before the next cycle
	}
	now := time.Now()
	live := map[string]bool{}
	count := 0
	for _, p := range st.Pending() {
		if p.Tier < 2 {
			continue
		}
		live[p.ID] = true
		count++
		tr.mu.Lock()
		until, snoozed := tr.snooze[p.ID]
		if snoozed && now.After(until) {
			delete(tr.snooze, p.ID)
			snoozed = false
		}
		eligible := !tr.shown[p.ID] && !snoozed
		if eligible {
			// WS2.6d: non-blocking send. A full queue no longer wedges
			// the poll goroutine (which also froze tooltip updates and
			// pruning); the id simply stays unshown and is retried on
			// the next tick.
			select {
			case queue <- p:
				tr.shown[p.ID] = true
			default:
			}
		}
		tr.mu.Unlock()
	}
	tr.mu.Lock()
	for id := range tr.shown {
		if !live[id] {
			delete(tr.shown, id)
		}
	}
	for id := range tr.snooze {
		if !live[id] {
			delete(tr.snooze, id)
		}
	}
	tr.mu.Unlock()
	updateTooltip(count)
}

func updateTooltip(count int) {
	if count == 0 {
		systray.SetTooltip("fbmcp — no pending approvals")
		return
	}
	systray.SetTooltip(fmt.Sprintf("fbmcp — %d pending approval(s)", count))
}
