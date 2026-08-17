package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	client := mcp.NewClient(&mcp.Implementation{Name: "t2", Version: "0"}, nil)
	wd, _ := os.Getwd()
	cmd := exec.Command(filepath.Join(wd, "fbmcp.exe"), "-config", "fbmcp.dev.yaml")
	sess, err := client.Connect(context.Background(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		fmt.Println("connect:", err)
		os.Exit(1)
	}
	defer sess.Close()
	call := func(name string, args map[string]any) string {
		res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return "PROTO-ERR: " + err.Error()
		}
		if len(res.Content) > 0 {
			if tc, ok := res.Content[0].(*mcp.TextContent); ok {
				return tc.Text
			}
		}
		return ""
	}
	re := regexp.MustCompile(`Request ID: ([0-9a-f]+)`)
	tokRe := regexp.MustCompile(`In-band token .*: ([0-9a-f]+)`)

	// 1) request Tier-2 restore replace (window already added to state.json by driver script)
	r := call("fb_restore_replace", map[string]any{"db": "spike5"})
	fmt.Println("T2 request:", first(r))
	m := re.FindStringSubmatch(r)
	if m == nil {
		fmt.Println("FAIL: no request id:\n" + r)
		os.Exit(1)
	}
	// 2) in-band confirm must be REJECTED (fuse 7)
	if tokRe.MatchString(r) {
		fmt.Println("FUSE PROBLEM: Tier-2 action offered an in-band token!")
	}
	ic := call("fb_confirm", map[string]any{"request_id": m[1], "token": "forged"})
	fmt.Println("T2 in-band:", first(ic))
	// 3) out-of-band: run fbmcp-approve CLI writing the marker
	fmt.Println("OOB approve...")
	ac := exec.Command(filepath.Join(wd, "fbmcp-approve.exe"), `C:/HQbirdData/output/fbmcp-spike/state`, m[1])
	out, err := ac.CombinedOutput()
	fmt.Printf("approve-cli: %v %s\n", err, first(string(out)))
	// 4) watcher confirms + dispatches; poll job
	time.Sleep(4 * time.Second)
	// verify outcome: the spike DB file timestamp changed (restored) and .pre-restore exists
	fi, err := os.Stat(`C:/HQbirdData/output/fbmcp-spike/spike_FB5.0.fdb`)
	fmt.Printf("db exists after restore: %v err=%v\n", fi != nil, err)
	if _, err := os.Stat(`C:/HQbirdData/output/fbmcp-spike/spike_FB5.0.fdb.pre-restore`); err == nil {
		fmt.Println("pre-restore copy: EXISTS")
	} else {
		fmt.Println("pre-restore copy: missing")
	}
}

func first(s string) string {
	if i := len(s); i > 120 {
		s = s[:120]
	}
	if j := 0; false {
		_ = j
	}
	for i, ch := range s {
		if ch == '\n' {
			return s[:i]
		}
		_ = i
	}
	return s
}
