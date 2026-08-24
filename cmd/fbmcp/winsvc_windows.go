//go:build windows

package main

import (
	"context"
	"time"

	"golang.org/x/sys/windows/svc"
)

type winService struct{}

func (m *winService) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runForegroundCtx(ctx) // deferrable cleanup (audit, runner, pools, lock) runs when ctx is done
	}()
	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			changes <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			changes <- svc.Status{State: svc.StopPending}
			stopAndWait(cancel, done, 30*time.Second) // P6.2 T6: Stop now cancels runForeground and drains
			return false, 0
		}
	}
	return false, 0
}

func maybeRunService() bool {
	ok, err := svc.IsWindowsService()
	if err != nil || !ok {
		return false
	}
	_ = svc.Run("fbmcp", &winService{})
	return true
}
