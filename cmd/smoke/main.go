package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

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
	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{"fb_ping", nil},
		{"fb_db_list", nil},
		{"fb_db_health", map[string]any{"db": "spike5"}},
		{"fb_db_health", map[string]any{"db": "ghost"}},
	} {
		res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: call.name, Arguments: call.args})
		if err != nil {
			fmt.Printf("%s -> protocol error: %v\n", call.name, err)
			continue
		}
		txt := ""
		if len(res.Content) > 0 {
			if tc, ok := res.Content[0].(*mcp.TextContent); ok {
				txt = tc.Text
			}
		}
		fmt.Printf("%s %v -> %q\n", call.name, call.args, txt)
	}
}
