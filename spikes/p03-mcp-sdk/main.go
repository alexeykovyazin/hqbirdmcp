// P0.3 MCP SDK spike — skeleton server over stdio with a ping tool and an
// elicitation round-trip. Subcommand "client" spawns the server and exercises
// it (self-test on any OS).
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type pingArgs struct{}

func serverMain() error {
	server := mcp.NewServer(&mcp.Implementation{Name: "fbmcp-spike", Version: "0.0.0"}, nil)

	mcp.AddTool(server, &mcp.Tool{Name: "ping", Description: "demo read tool"}, func(ctx context.Context, req *mcp.CallToolRequest, _ pingArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
	})

	// Elicitation round-trip (protocol 2026-07-28 / SEP-2322): the tool returns
	// InputRequests; a capable client elicits from the human and retries.
	mcp.AddTool(server, &mcp.Tool{Name: "demo_gated", Description: "demo Tier-1 gated tool (elicitation probe)"}, func(ctx context.Context, req *mcp.CallToolRequest, _ pingArgs) (*mcp.CallToolResult, any, error) {
		if r := req.Params.InputResponses["confirm"]; r != nil {
			if er, ok := r.(*mcp.ElicitResult); ok && er.Content["approve"] == true {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "APPROVED and executed (demo)"}}}, nil, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "DENIED by human"}}}, nil, nil
		}
		return &mcp.CallToolResult{
			InputRequests: mcp.InputRequestMap{
				"confirm": &mcp.ElicitParams{
					Message: "Approve demo mutation? (spike)",
					RequestedSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"approve": map[string]any{"type": "boolean", "title": "Approve"},
						},
						"required": []string{"approve"},
					},
				},
			},
		}, nil, nil
	})

	return server.Run(context.Background(), &mcp.StdioTransport{})
}

func clientMain() error {
	client := mcp.NewClient(&mcp.Implementation{Name: "spike-client", Version: "0"}, &mcp.ClientOptions{
		ElicitationHandler: func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Content: map[string]any{"approve": true}}, nil
		},
	})
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), "FBMCP_SPIKE_ROLE=server")
	session, err := client.Connect(context.Background(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return err
	}
	defer session.Close()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ping"})
	fmt.Printf("ping -> err=%v", err)
	if err == nil && len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			fmt.Printf(" text=%q", tc.Text)
		}
	}
	fmt.Println()

	res, err = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "demo_gated"})
	fmt.Printf("demo_gated -> err=%v", err)
	if err == nil && len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			fmt.Printf(" text=%q", tc.Text)
		}
	}
	fmt.Println()
	return nil
}

func main() {
	var err error
	if os.Getenv("FBMCP_SPIKE_ROLE") == "server" {
		err = serverMain()
	} else if len(os.Args) > 1 && os.Args[1] == "server" {
		err = serverMain()
	} else {
		err = clientMain()
	}
	if err != nil {
		log.Fatal(err)
	}
}
