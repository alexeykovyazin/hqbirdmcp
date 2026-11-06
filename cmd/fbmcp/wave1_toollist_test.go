package main

import (
	"context"
	"net"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/backupsvc"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/dbpool"
	"github.com/aleks/fbmcp/internal/policy"
	"github.com/aleks/fbmcp/internal/workflows"
)

// UX review W1.8: list only usable tools, explain the rules once.

func TestToolVisible(t *testing.T) {
	tiers := toolTiers(toolMeta)
	plain := &config.Config{Databases: []config.Database{{ID: "a"}}}
	withAll := &config.Config{Databases: []config.Database{{ID: "a", MigrationsDir: "m"}}, Agents: []config.Agent{{ID: "ag"}}}
	ops := policy.Identity{Name: "ops", MaxTier: 2}
	reader := policy.Identity{Name: "reader", MaxTier: 0}
	cases := []struct {
		tool string
		id   policy.Identity
		cfg  *config.Config
		want bool
	}{
		{"fb_db_drop", ops, withAll, false},          // Tier 3: always denied
		{"fb_restore_replace", reader, plain, false}, // above the ceiling
		{"fb_restore_replace", ops, plain, true},
		{"fb_query", reader, plain, true},
		{"fb_confirm", reader, plain, false},
		{"fb_confirm", ops, plain, true},
		{"fb_agent_status", ops, plain, false}, // no agents configured
		{"fb_agent_status", ops, withAll, true},
		{"fb_agent_discover", ops, plain, true}, // the way to the first agent
		{"fb_migration_apply", ops, plain, false},
		{"fb_migration_apply", ops, withAll, true},
	}
	for _, c := range cases {
		if got := toolVisible(c.tool, c.id, c.cfg, tiers); got != c.want {
			t.Errorf("%s for %s: visible=%v, want %v", c.tool, c.id.Name, got, c.want)
		}
	}
}

func TestToolListFilterMiddleware(t *testing.T) {
	cfg := testCfg(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "filter", Version: "0"}, &mcp.ServerOptions{Instructions: serverInstructions})
	server.AddReceivingMiddleware(toolListFilter(config.NewHandle(cfg), toolMeta))
	type noArgs struct{}
	h := func(ctx context.Context, req *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
		return text("ok"), nil, nil
	}
	for _, name := range []string{"fb_ping", "fb_db_drop", "fb_agent_status", "fb_agent_discover"} {
		mcp.AddTool(server, &mcp.Tool{Name: name, Description: "x"}, h)
	}
	sess := connectInProcess(t, server)
	res, err := sess.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	if got := strings.Join(names, ","); got != "fb_agent_discover,fb_ping" {
		t.Fatalf("listed tools = %s, want fb_agent_discover,fb_ping", got)
	}
	if !strings.Contains(sess.InitializeResult().Instructions, "Want -> use") {
		t.Fatal("server instructions missing from initialize")
	}
}

func TestServerInstructionsNameRealTools(t *testing.T) {
	known := map[string]bool{}
	for _, m := range toolMeta {
		known[m.Name] = true
	}
	for _, m := range regexp.MustCompile(`\bfb_[a-z_]+`).FindAllString(serverInstructions, -1) {
		if strings.HasSuffix(m, "_") {
			continue // a prefix such as "fb_agent_ tools"
		}
		if !known[m] {
			t.Errorf("server instructions name %s, which is not a tool", m)
		}
	}
}

// Plan and ADR references mean nothing to the model or the user (C14).
var jargon = regexp.MustCompile(`ADR-?[0-9]|phase[0-9]+_plan|\b[A-Z][0-9]+\.[0-9]+\b|\bWS[0-9]+`)

func TestToolTextHasNoPlanJargon(t *testing.T) {
	for _, tl := range allRegisteredTools(t) {
		texts := []string{tl.Description}
		if s, ok := tl.InputSchema.(map[string]any); ok {
			if props, ok := s["properties"].(map[string]any); ok {
				for _, p := range props {
					if pm, ok := p.(map[string]any); ok {
						if d, ok := pm["description"].(string); ok {
							texts = append(texts, d)
						}
					}
				}
			}
		}
		for _, s := range texts {
			if m := jargon.FindString(s); m != "" {
				t.Errorf("%s: plan jargon %q in %q", tl.Name, m, s)
			}
		}
	}
	// main.go registers the core tools inline: scan its literals too.
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`(?:Description: |jsonschema:)"([^"]*)"`).FindAllStringSubmatch(string(src), -1) {
		if j := jargon.FindString(m[1]); j != "" {
			t.Errorf("main.go: plan jargon %q in %q", j, m[1])
		}
	}
}

// allRegisteredTools lists the tools of the register* functions (the same
// set the drift test uses), unfiltered.
func allRegisteredTools(t *testing.T) []*mcp.Tool {
	t.Helper()
	gt := newTestGT(t)
	gt.wf = workflows.New(gt.st)
	gt.pools = dbpool.NewManager(gt.cfg)
	gt.traces = map[string]*backupsvc.LiveTrace{}
	server := mcp.NewServer(&mcp.Implementation{Name: "all", Version: "0"}, nil)
	registerP3Tools(server, gt)
	gt.registerRestore(server)
	registerP4Tools(server, gt)
	registerP5Tools(server, gt)
	registerAgentTools(server, gt)
	registerAgentDiscoverTools(server, gt)
	registerPhase11Tools(server, gt)
	registerDBTool(server, gt)
	res, err := connectInProcess(t, server).ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	return res.Tools
}

func connectInProcess(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	srvConn, cliConn := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	go server.Run(ctx, &mcp.IOTransport{Reader: srvConn, Writer: srvConn})
	t.Cleanup(func() { cliConn.Close() })
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: cliConn, Writer: cliConn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

func TestRelistOnlyWhenToolSetChanges(t *testing.T) {
	gt := newTestGT(t)
	n := 0
	gt.relist = func() { n++ }
	cfg := gt.live()
	gt.toolKey = toolListKey(cfg)
	gt.relistIfChanged(cfg)
	if n != 0 {
		t.Fatalf("relist without a change: %d", n)
	}
	withAgent := *cfg
	withAgent.Agents = []config.Agent{{ID: "ag"}}
	gt.relistIfChanged(&withAgent)
	gt.relistIfChanged(&withAgent)
	if n != 1 {
		t.Fatalf("relist count after adding an agent = %d, want 1", n)
	}
}
