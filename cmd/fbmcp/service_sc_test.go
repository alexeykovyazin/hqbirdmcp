package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/adminexec"
)

// fakeSCM answers sc.exe like the SCM: stop and start return at once with a
// pending state that settles after a few queries; a start before STOPPED
// fails with 1056.
type fakeSCM struct {
	state, settle int // settle: queries left in a pending state
	early         int // starts sent before STOPPED
}

func (f *fakeSCM) run(_ context.Context, args ...string) adminexec.Result {
	switch args[0] {
	case "query":
		out := fmt.Sprintf("SERVICE_NAME: fb\n        STATE              : %d  X\n", f.state)
		if f.state == 2 || f.state == 3 {
			if f.settle--; f.settle <= 0 {
				f.state = map[int]int{2: scRunning, 3: scStopped}[f.state]
			}
		}
		return adminexec.Result{Output: out}
	case "stop":
		if f.state == scStopped {
			return adminexec.Result{Output: "[SC] ControlService FAILED 1062:", Err: errors.New("exit status 1062")}
		}
		if f.state == 3 {
			return adminexec.Result{Output: "[SC] ControlService FAILED 1061:", Err: errors.New("exit status 1061")}
		}
		f.state, f.settle = 3, 3
		return adminexec.Result{Output: "STATE : 3 STOP_PENDING"}
	case "start":
		if f.state != scStopped {
			f.early++
			return adminexec.Result{Output: "[SC] StartService FAILED 1056:", Err: errors.New("exit status 1056")}
		}
		f.state, f.settle = 2, 2
		return adminexec.Result{Output: "STATE : 2 START_PENDING"}
	}
	return adminexec.Result{Err: fmt.Errorf("unexpected sc %v", args)}
}

func useFakeSCM(t *testing.T, state int) *fakeSCM {
	f := &fakeSCM{state: state}
	oldRun, oldPoll, oldLimit := scRun, scPoll, scLimit
	scRun, scPoll, scLimit = f.run, time.Millisecond, time.Second
	t.Cleanup(func() { scRun, scPoll, scLimit = oldRun, oldPoll, oldLimit })
	return f
}

// Phase 12 test W: the restart sent the start during STOP_PENDING, got
// 1056 and left Firebird stopped.
func TestServiceRestartWaitsForStopped(t *testing.T) {
	ctx := context.Background()
	f := useFakeSCM(t, scRunning)
	out, err := controlServiceSC(ctx, "fb", "restart")
	if err != nil || f.state != scRunning || f.early != 0 {
		t.Fatalf("restart: %q %v; state %d, early starts %d", out, err, f.state, f.early)
	}
	f = useFakeSCM(t, scStopped) // a restart of a stopped service starts it
	if out, err := controlServiceSC(ctx, "fb", "restart"); err != nil || f.state != scRunning {
		t.Fatalf("restart from stopped: %q %v; state %d", out, err, f.state)
	}
	f = useFakeSCM(t, scRunning)
	if out, err := controlServiceSC(ctx, "fb", "start"); err != nil || !strings.Contains(out, "already running") {
		t.Fatalf("start while running: %q %v", out, err)
	}
	f = useFakeSCM(t, scRunning)
	if out, err := controlServiceSC(ctx, "fb", "stop"); err != nil || f.state != scStopped {
		t.Fatalf("stop: %q %v; state %d", out, err, f.state)
	}
	f = useFakeSCM(t, scRunning)
	f.settle = 1 << 30 // a stop that never ends
	f.state = 3
	if _, err := controlServiceSC(ctx, "fb", "restart"); err == nil || !strings.Contains(err.Error(), "STOP_PENDING") {
		t.Fatalf("hung stop: %v", err)
	}
}
