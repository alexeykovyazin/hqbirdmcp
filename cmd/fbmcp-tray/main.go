//go:build windows

// fbmcp-tray — the out-of-band confirmation surface as a Windows tray app
// (§5.5c, Tier ≥ 2). Watches the same state dir the running fbmcp server
// reads/writes and pops a native Approve/Deny dialog for each Tier ≥ 2
// pending action, writing the same approvals/denials markers the
// fbmcpctl approve CLI already writes. The MCP client
// cannot reach this surface.
package main

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/getlantern/systray"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/state"
)

//go:embed assets/tray.ico
var trayIcon []byte

func main() {
	// Single-instance guard (WS2.6f): two trays would double-prompt the
	// operator for the same pending action.
	if !acquireSingleInstance() {
		msgBox("fbmcp-tray", "Another fbmcp-tray instance is already running.")
		os.Exit(1)
	}

	// Default config path is exe-relative, not CWD-relative (WS2.6b): this
	// is a GUI binary launched from a Startup shortcut where the CWD is
	// unpredictable and stderr goes nowhere.
	cfgPath := "fbmcp.yaml"
	if exe, err := os.Executable(); err == nil {
		cfgPath = filepath.Join(filepath.Dir(exe), "fbmcp.yaml")
	}
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		// WS2.6a: a silent exit is indistinguishable from "running with
		// nothing pending" — tell the operator on screen, not just stderr.
		fmt.Fprintln(os.Stderr, "fbmcp-tray: config:", err)
		msgBox("fbmcp-tray — configuration error", fmt.Sprintf("Cannot start: %v\n\nConfig path: %s", err, cfgPath))
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

	tr := newTracker()
	queue := make(chan state.PendingAction, 16)
	go pollLoop(stateDir, tr, queue)
	go dialogWorker(stateDir, tr, queue)

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
