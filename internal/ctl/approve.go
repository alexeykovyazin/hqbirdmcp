package ctl

// fbmcp ctl approve (UX review W1.5): list, approve or deny the pending
// Tier 2 requests. It writes the same marker files as the tray; the kernel
// consumes them within a few seconds.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/aleks/fbmcp/internal/attach"
	"github.com/aleks/fbmcp/internal/gate"
	"github.com/aleks/fbmcp/internal/state"
)

const approveUsage = `usage: fbmcp ctl approve [--config FILE] [request_id] [--yes]
       fbmcp ctl approve [--config FILE] --deny <request_id> [--reason TEXT] [--yes]
       fbmcp ctl approve <state_dir> [request_id]   (old form)
  Without a request_id: list the pending requests.
  It asks "[y/N]" before it writes; --yes does not ask.
  ` + configHelp + "\n"

// promptIn and kernelUp are variables for the tests.
var (
	promptIn io.Reader = os.Stdin
	kernelUp           = func(stateDir string) bool {
		c, err := attach.Dial(stateDir, 2*time.Second)
		if err != nil {
			return false
		}
		c.Close()
		return true
	}
)

func cmdApprove(args []string) int {
	explicit, args := takeConfigFlag(args)
	var stateDir, id, deny, reason string
	yes := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--yes" || a == "-y":
			yes = true
		case a == "--deny" && i+1 < len(args):
			deny = args[i+1]
			i++
		case strings.HasPrefix(a, "--deny="):
			deny = strings.TrimPrefix(a, "--deny=")
		case a == "--reason" && i+1 < len(args):
			reason = args[i+1]
			i++
		case strings.HasPrefix(a, "--reason="):
			reason = strings.TrimPrefix(a, "--reason=")
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "approve: unknown flag %s\n%s", a, approveUsage)
			return 2
		case stateDir == "" && id == "" && explicit == "" && isDir(a):
			stateDir = a // the old form: approve <state_dir> [request_id]
		case id == "":
			id = a
		default:
			fmt.Fprint(os.Stderr, approveUsage)
			return 2
		}
	}
	if deny != "" && id != "" {
		fmt.Fprintf(os.Stderr, "approve: give a request_id or --deny <request_id>, not both\n%s", approveUsage)
		return 2
	}
	if reason != "" && deny == "" {
		fmt.Fprintln(os.Stderr, "approve: --reason goes with --deny")
		return 2
	}

	// cmd is how the next commands are spelled for this operator.
	cmd := "fbmcp ctl approve"
	if stateDir != "" {
		cmd += " " + quoteArg(stateDir)
	} else {
		cfg, _, err := loadConfig(explicit)
		if err != nil {
			fmt.Fprintln(os.Stderr, "approve:", err)
			return 1
		}
		stateDir = cfg.State.Dir
		if explicit != "" {
			cmd += " --config " + quoteArg(explicit)
		}
	}
	// Never create the folder: a typo would make an empty queue that
	// nothing reads, and "no pending requests" would look true.
	if !isDir(stateDir) {
		fmt.Fprintf(os.Stderr, "approve: state folder %s does not exist\nFix: check state.dir in fbmcp.yaml (or --config), or start fbmcp once so it makes the folder\n", stateDir)
		return 1
	}
	st, err := state.Open(stateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "approve: state:", err)
		return 1
	}
	pending := st.Pending()

	target := id
	if deny != "" {
		target = deny
	}
	if target == "" {
		if len(pending) == 0 {
			fmt.Println("no pending requests in", stateDir)
			return 0
		}
		for _, p := range pending {
			fmt.Print(gate.ImpactStatement(p))
			fmt.Printf("Approve: %s %s\nDeny:    %s --deny %s --reason \"...\"\n---\n", cmd, p.ID, cmd, p.ID)
		}
		return 0
	}

	var p state.PendingAction
	found := false
	for _, q := range pending {
		if q.ID == target {
			p, found = q, true
		}
	}
	if !found {
		// lab 2026-11-04: an expired id used to print "no pending actions"
		// with exit 0, so the operator took it for an approval.
		fmt.Printf("no such pending request: %s (a request expires after %s; an approved, denied or expired one leaves the list)\nNext: %s  (lists the pending requests)\n", target, gate.DefaultTTL, cmd)
		return 1
	}
	fmt.Print(gate.ImpactStatement(p))

	up := kernelUp(stateDir)
	if !up && deny == "" {
		fmt.Printf("WARNING: no running fbmcp answers for %s.\n", stateDir)
		fmt.Println("The arguments of a request live only in the memory of the fbmcp that took it. If that fbmcp stopped, this request cannot run: the approval fails with \"arguments lost\" and the client must ask again.")
	}
	verb, sub, verdict := "Approve", "approvals", gate.MarkerApproved
	if deny != "" {
		verb, sub, verdict = "Deny", "denials", gate.MarkerDenied
	}
	if !yes {
		fmt.Printf("%s request %s? [y/N] ", verb, target)
		if !answerYes(promptIn) {
			fmt.Println("not changed")
			return 1
		}
	}
	dir := filepath.Join(stateDir, sub)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "approve: write marker:", err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(dir, target), []byte(gate.MarkerBody(verdict, osUser(), reason)), 0o640); err != nil {
		fmt.Fprintln(os.Stderr, "approve: write marker:", err)
		return 1
	}
	switch {
	case deny != "":
		fmt.Println("denied. fbmcp drops the request within a few seconds.")
	case up:
		fmt.Printf("approved. fbmcp runs the action within a few seconds.\nNext: the client calls fb_job_status with request_id=%q\n", target)
	default:
		fmt.Println("approval written, but see the warning above: when fbmcp starts, it fails this request. Ask the client to request the action again.")
	}
	return 0
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func answerYes(r io.Reader) bool {
	line, _ := bufio.NewReader(r).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// osUser names the operator in the marker (and so in the audit log).
func osUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	for _, k := range []string{"USERNAME", "USER"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func quoteArg(s string) string {
	if strings.ContainsAny(s, " \t\"") {
		return `"` + s + `"`
	}
	return s
}
