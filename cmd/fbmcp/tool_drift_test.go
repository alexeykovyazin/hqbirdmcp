package main

import (
	"context"
	"net"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/backupsvc"
	"github.com/aleks/fbmcp/internal/dbpool"
	"github.com/aleks/fbmcp/internal/workflows"
)

// P2.8 (test_plan): three-way surface agreement — every tool the server
// exposes must exist in toolMeta (== policy.SystemTools) and vice versa.
//
// Two registration forms exist in production: register* functions (checked
// here through a real tools/list over an in-process MCP session — hermetic,
// registration touches no pools) and the core tools mcp.AddTool'ed inline in
// main.go (checked by scanning main.go for AddTool names; extracting them is
// a possible follow-up refactor).

func TestToolRegistryThreeWayAgreement(t *testing.T) {
	// 1. live registration through the same functions production calls
	gt := newTestGT(t)
	gt.wf = workflows.New(gt.st) // registerP5Tools registers its workflow types
	gt.pools = dbpool.NewManager(gt.cfg)
	gt.traces = map[string]*backupsvc.LiveTrace{}
	server := mcp.NewServer(&mcp.Implementation{Name: "drift", Version: "0"}, nil)
	registerP3Tools(server, gt)
	gt.registerRestore(server)
	registerP4Tools(server, gt)
	registerP5Tools(server, gt)
	if registerExtra != nil {
		registerExtra(server, gt.cfg, gt.pools, nil, gt.aud, gt.st)
	}
	registerDBTool(server, gt)

	srvConn, cliConn := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go server.Run(ctx, &mcp.IOTransport{Reader: srvConn, Writer: srvConn})
	defer cliConn.Close()
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "drift-client", Version: "0"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: cliConn, Writer: cliConn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	res, err := sess.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	live := map[string]bool{}
	for _, tl := range res.Tools {
		live[tl.Name] = true
	}

	// 2. core tools registered inline in main.go (source scan — same file
	// production builds from, so a rename/typo breaks this test)
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`AddTool\(server, &mcp\.Tool\{Name: "(fb_[a-z_]+)"`).FindAllStringSubmatch(string(src), -1) {
		live[m[1]] = true
	}

	// 3. the policy surface
	meta := map[string]bool{}
	for _, m := range toolMeta {
		if meta[m.Name] {
			t.Fatalf("duplicate toolMeta entry %q", m.Name)
		}
		meta[m.Name] = true
	}

	var extra, missing []string
	for name := range live {
		if !meta[name] {
			extra = append(extra, name)
		}
	}
	for name := range meta {
		if !live[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	if len(extra) > 0 {
		t.Fatalf("registered but not in toolMeta: %s", strings.Join(extra, ", "))
	}
	if len(missing) > 0 {
		t.Fatalf("in toolMeta but never registered: %s", strings.Join(missing, ", "))
	}

	// the policy Engine must know exactly the same surface
	eng := gt.eng
	known := map[string]bool{}
	for _, m := range eng.Tools() {
		known[m.Name] = true
	}
	for name := range meta {
		if !known[name] {
			t.Fatalf("toolMeta %q unknown to the policy engine", name)
		}
	}
	if len(known) != len(meta) {
		t.Fatalf("policy engine knows %d tools, toolMeta has %d", len(known), len(meta))
	}
	if len(live) < 60 {
		t.Fatalf("suspiciously small live surface (%d tools)", len(live))
	}
}
