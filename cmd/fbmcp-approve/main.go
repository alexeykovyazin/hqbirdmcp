// fbmcp-approve — the out-of-band confirmation CLI (§5.5c, Tier ≥ 2).
// Lists pending actions and/or writes an approval marker that the running
// fbmcp server picks up. The MCP client cannot invoke this surface.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/aleks/fbmcp/internal/gate"
	"github.com/aleks/fbmcp/internal/state"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: fbmcp-approve <state_dir> [request_id]")
		os.Exit(2)
	}
	dir := os.Args[1]
	st, err := state.Open(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "state:", err)
		os.Exit(1)
	}
	pending := st.Pending()
	if len(pending) == 0 {
		fmt.Println("no pending actions")
		return
	}
	if len(os.Args) == 2 {
		for _, p := range pending {
			fmt.Println(gate.ImpactStatement(p))
			fmt.Println("---")
		}
		fmt.Println("run: fbmcp-approve <state_dir> <request_id> to approve")
		return
	}
	id := os.Args[2]
	found := false
	for _, p := range pending {
		if p.ID == id {
			found = true
			fmt.Print(gate.ImpactStatement(p))
		}
	}
	if !found {
		fmt.Println("no such pending action:", id)
		os.Exit(1)
	}
	// approval marker: the server's watcher consumes it (single-writer respected)
	approvals := filepath.Join(dir, "approvals")
	os.MkdirAll(approvals, 0o755)
	if err := os.WriteFile(filepath.Join(approvals, id), []byte("approved-by-operator\n"), 0o640); err != nil {
		fmt.Fprintln(os.Stderr, "write marker:", err)
		os.Exit(1)
	}
	fmt.Println("approval recorded — the server will confirm and execute the action")
}
