package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	client := mcp.NewClient(&mcp.Implementation{Name: "smoke", Version: "0"}, nil)
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

	fmt.Println("ping:", call("fb_ping", nil))
	fmt.Println("plan:", firstLine(call("fb_analyze_query", map[string]any{"db": "spike5", "query": "SELECT * FROM RDB$RELATIONS WHERE RDB$RELATION_NAME = 'CUSTOMER'"})))
	fmt.Println("idx:", firstLine(call("fb_index_stats", map[string]any{"db": "spike5"})))
	fmt.Println("schema:", firstLine(call("fb_schema_list", map[string]any{"db": "spike5"})))
	fmt.Println("describe:", firstLine(call("fb_describe", map[string]any{"db": "spike5", "table": "SPIKE_RO"})))
	fmt.Println("sample:", strings.ReplaceAll(call("fb_activity_sample", map[string]any{"db": "spike5", "seconds": 2}), "\n", " | "))
	fmt.Println("info5:", strings.ReplaceAll(call("fb_info", map[string]any{"db": "spike5"}), "\n", " | "))
	fmt.Println("sess5:", firstLine(call("fb_sessions", map[string]any{"db": "spike5"})))
	fmt.Println("tx5:", strings.ReplaceAll(call("fb_transactions", map[string]any{"db": "spike5"}), "\n", " | "))
	fmt.Println("info-ghost:", firstLine(call("fb_info", map[string]any{"db": "ghost"})))
	fmt.Println("list:", strings.ReplaceAll(call("fb_db_list", nil), "\n", " | "))

	// gated demo: no token => pending only, no effect
	pending := call("fb_demo_write", map[string]any{"db": "spike5"})
	fmt.Println("request:", firstLine(pending))

	re := regexp.MustCompile(`Request ID: ([0-9a-f]+)`)
	m := re.FindStringSubmatch(pending)
	if m == nil {
		fmt.Println("FAIL: no request id in pending action")
		os.Exit(1)
	}
	reqID := m[1]

	tokRe := regexp.MustCompile(`token \(Tier 1 only\): ([0-9a-f]+)`)
	tm := tokRe.FindStringSubmatch(pending)

	// confirm without token must fail
	fmt.Println("confirm-no-token:", firstLine(call("fb_confirm", map[string]any{"request_id": reqID})))

	// confirm with token -> job
	confirmed := call("fb_confirm", map[string]any{"request_id": reqID, "token": tm[1]})
	fmt.Println("confirm:", firstLine(confirmed))

	// replay must fail
	fmt.Println("replay:", firstLine(call("fb_confirm", map[string]any{"request_id": reqID, "token": tm[1]})))

	jobRe := regexp.MustCompile(`job (j[0-9]+)`)
	jm := jobRe.FindStringSubmatch(confirmed)
	if jm != nil {
		fmt.Println("job:", firstLine(call("fb_job_status", map[string]any{"job_id": jm[1]})))
	}
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}
