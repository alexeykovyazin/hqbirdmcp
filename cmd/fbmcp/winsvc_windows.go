//go:build windows

package main

import (
	"golang.org/x/sys/windows/svc"
)

type winService struct{}

func (m *winService) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}
	go runForeground()
	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			changes <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			changes <- svc.Status{State: svc.StopPending}
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
