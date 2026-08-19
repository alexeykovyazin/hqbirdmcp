// fb_write E2E test harness (dev only).
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	client := mcp.NewClient(&mcp.Implementation{Name: "w", Version: "0"}, nil)
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
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			return tc.Text
		}
		return ""
	}
	re := regexp.MustCompile(`Request ID: ([0-9a-f]+)`)
	tokRe := regexp.MustCompile(`In-band token .*: ([0-9a-f]+)`)
	jobRe := regexp.MustCompile(`job (j[0-9]+)`)

	fmt.Println("garbage:", first(call("fb_write", map[string]any{"db": "spike5", "sql": "~~~ nonsense ~~~"})))
	fmt.Println("drop-db:", first(call("fb_write", map[string]any{"db": "spike5", "sql": "DROP DATABASE"})))

	sql := "CREATE TABLE W_TEST (ID INT, VAL VARCHAR(10)); INSERT INTO W_TEST VALUES (1, 'a')"
	r := call("fb_write", map[string]any{"db": "spike5", "sql": sql})
	fmt.Println("preview-first-3:", strings.ReplaceAll(firstN(r, 3), "\n", " | "))
	m := re.FindStringSubmatch(r)
	tk := tokRe.FindStringSubmatch(r)
	if m == nil || tk == nil {
		fmt.Println("FAIL:\n" + r)
		os.Exit(1)
	}
	c := call("fb_confirm", map[string]any{"request_id": m[1], "token": tk[1]})
	fmt.Println("confirm:", first(c))
	if jm := jobRe.FindStringSubmatch(c); jm != nil {
		time.Sleep(2 * time.Second)
		fmt.Println("job:", first(call("fb_job_status", map[string]any{"job_id": jm[1]})))
	}
}

func first(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func firstN(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	return strings.Join(lines[:n], "\n")
}
