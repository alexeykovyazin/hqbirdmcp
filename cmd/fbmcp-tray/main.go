//go:build windows

// fbmcp-tray — the out-of-band confirmation surface as a Windows tray app
// (§5.5c, Tier ≥ 2). Watches the same state dir the running fbmcp server
// reads/writes and pops a native Approve/Deny dialog for each Tier ≥ 2
// pending action, writing the same approvals/denials markers the
// fbmcp-approve / fbmcpctl approve CLIs already write. The MCP client
// cannot reach this surface.
package main

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"

	"github.com/getlantern/systray"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/state"
)

//go:embed assets/tray.ico
var trayIcon []byte

func main() {
	cfgPath := "fbmcp.yaml"
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fbmcp-tray: config:", err)
		os.Exit(1)
	}
	stateDir := cfg.State.Dir

	systray.Run(func() { onReady(stateDir) }, func() {})
}

func onReady(stateDir string) {
	systray.SetIcon(trayIcon)
	systray.SetTooltip("fbmcp — no pending approvals")

	mOpen := systray.AddMenuItem("Open state folder", "Open the fbmcp state directory")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Quit fbmcp-tray")

	queue := make(chan state.PendingAction, 16)
	go pollLoop(stateDir, queue)
	go dialogWorker(stateDir, queue)

	go func() {
		for {
			select {
			case <-mOpen.ClickedCh:
				_ = exec.Command("explorer", stateDir).Start()
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}
