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
	"time"

	"github.com/getlantern/systray"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/state"
)

//go:embed assets/tray.ico
var trayIcon []byte

func main() {
	// A windowsgui binary dies without any visible trace when a goroutine
	// panics, and WS2.6a: a silent exit is indistinguishable from "running
	// with nothing pending" — surface main-goroutine crashes before
	// exiting. Worker-goroutine panics are recovered at their own loops.
	defer func() {
		if r := recover(); r != nil {
			msgBox("fbmcp-tray — crashed", fmt.Sprintf("fbmcp-tray exited unexpectedly: %v\n\nPending approvals will not pop until it is restarted.", r))
			os.Exit(1)
		}
	}()

	// Single-instance guard (WS2.6f): two trays would double-prompt the
	// operator for the same pending action.
	if !acquireSingleInstance() {
		msgBox("fbmcp-tray", "Another fbmcp-tray instance is already running.")
		os.Exit(1)
	}

	// Default config path is exe-relative, not CWD-relative (WS2.6b): this
	// is a GUI binary launched from a Startup shortcut where the CWD is
	// unpredictable and stderr goes nowhere.
	// --console reattaches stdout/stderr to the parent terminal for
	// diagnostics; every other argument stays the config path.
	var args []string
	for _, a := range os.Args[1:] {
		if a == "--console" {
			attachConsole()
			continue
		}
		args = append(args, a)
	}
	cfgPath := "fbmcp.yaml"
	if exe, err := os.Executable(); err == nil {
		cfgPath = filepath.Join(filepath.Dir(exe), "fbmcp.yaml")
	}
	if len(args) > 0 {
		cfgPath = args[0]
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
	trayLogPath = filepath.Join(stateDir, "tray.log")

	systray.Run(func() { onReady(stateDir) }, func() {})
}

// trayLogPath is the diagnostic sink for a windowsgui binary: stderr goes
// nowhere unless --console was passed, so recovered panics and dialog
// failures append here instead. Best-effort; empty until config loads.
var trayLogPath string

func logErr(format string, args ...any) {
	if trayLogPath == "" {
		return
	}
	f, err := os.OpenFile(trayLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return // nothing to log into — nothing else to do
	}
	defer f.Close()
	fmt.Fprintf(f, time.Now().Format("2006-01-02 15:04:05.000 ")+format+"\n", args...)
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
