//go:build windows

package main

import (
	"fmt"
	"time"

	"github.com/getlantern/systray"

	"github.com/aleks/fbmcp/internal/state"
)

const pollInterval = 3 * time.Second

// pollLoop re-opens the state store every tick — state.Open reads
// state.json once per call (it is not a live handle), so this is the only
// way a second process notices pending actions the running fbmcp server
// added after this process started. New Tier >= 2 ids are pushed to queue
// once each; the shown-set is pruned once an id leaves Pending() (consumed
// by approval/denial/expiry) so it never grows unbounded and, if the
// operator dismissed the dialog without a button, the id can be re-shown
// after a config reload cycle recreates it under a fresh id (ids are never
// reused — gate.newID is 12 random bytes).
func pollLoop(stateDir string, queue chan<- state.PendingAction) {
	shown := map[string]bool{}
	for {
		st, err := state.Open(stateDir)
		if err == nil {
			live := map[string]bool{}
			count := 0
			for _, p := range st.Pending() {
				if p.Tier < 2 {
					continue
				}
				live[p.ID] = true
				count++
				if !shown[p.ID] {
					shown[p.ID] = true
					queue <- p
				}
			}
			for id := range shown {
				if !live[id] {
					delete(shown, id)
				}
			}
			updateTooltip(count)
		}
		time.Sleep(pollInterval)
	}
}

func updateTooltip(count int) {
	if count == 0 {
		systray.SetTooltip("fbmcp — no pending approvals")
		return
	}
	systray.SetTooltip(fmt.Sprintf("fbmcp — %d pending approval(s)", count))
}
