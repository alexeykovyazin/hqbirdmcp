// cmd/fbmcp — the fbmcp MCP server (Phase 1 skeleton).
//
// Usage: fbmcp --config fbmcp.yaml   (stdio transport, local mode)
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/dbpool"
)

func main() {
	cfgPath := flag.String("config", "fbmcp.yaml", "path to fbmcp configuration")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fbmcp: %v\n", err)
		os.Exit(1)
	}
	aud, err := audit.Open(cfg.State.Dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fbmcp: audit: %v\n", err)
		os.Exit(1)
	}
	defer aud.Close()
	pools := dbpool.NewManager(cfg)
	defer pools.Close()

	server := mcp.NewServer(&mcp.Implementation{Name: "fbmcp", Version: "0.1.0"}, nil)

	type noArgs struct{}
	type dbArg struct {
		Db string `json:"db" jsonschema:"registry id of the database"`
	}

	mcp.AddTool(server, &mcp.Tool{Name: "fb_ping", Description: "liveness probe"}, func(ctx context.Context, req *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
		return text("pong"), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{Name: "fb_db_health", Description: "Tier 0: probe a registered database (per-DB degraded mode check)"}, func(ctx context.Context, req *mcp.CallToolRequest, a dbArg) (*mcp.CallToolResult, any, error) {
		if err := pools.Health(ctx, a.Db); err != nil {
			aud.Log(audit.Entry{Identity: "local", Database: a.Db, Tool: "fb_db_health", Tier: 0, Decision: "error", Detail: map[string]interface{}{"error": err.Error()}})
			return text("offline: " + err.Error()), nil, nil
		}
		aud.Log(audit.Entry{Identity: "local", Database: a.Db, Tool: "fb_db_health", Tier: 0, Decision: "allow"})
		return text("online"), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{Name: "fb_db_list", Description: "Tier 0: list registered databases with health"}, func(ctx context.Context, req *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
		out := ""
		for _, db := range cfg.Databases {
			state := "online"
			if err := pools.Health(ctx, db.ID); err != nil {
				state = "offline"
			}
			out += fmt.Sprintf("- %s [%s]\n", db.ID, state)
		}
		return text(out), nil, nil
	})

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("fbmcp: server: %v", err)
	}
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}
