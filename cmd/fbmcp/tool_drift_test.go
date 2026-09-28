package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/backupsvc"
	"github.com/aleks/fbmcp/internal/dbpool"
	"github.com/aleks/fbmcp/internal/uigrid"
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
	registerAgentTools(server, gt)
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

// TestREADMEToolCounts (phase10 §11 D3) guards README's count statement —
// the one line a human reads for the surface size. The phase-9 narrative
// keeps its historical numbers by design; only the current-count line is
// pinned here.
func TestREADMEToolCounts(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	b, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	flat := strings.Join(strings.Fields(string(b)), " ")
	var total, t0, t1, t2, t3 int
	for _, m := range toolMeta {
		total++
		switch m.Tier {
		case 0:
			t0++
		case 1:
			t1++
		case 2:
			t2++
		case 3:
			t3++
		}
	}
	re := regexp.MustCompile(`(\d+) tools in total \((\d+) read-only, (\d+) Tier 1, (\d+) Tier 2, (\d+) disabled-by-design Tier 3`)
	m := re.FindStringSubmatch(flat)
	if m == nil {
		t.Fatal("README 'N tools in total (...)' count line missing or reworded")
	}
	want := []string{
		strconv.Itoa(total), strconv.Itoa(t0), strconv.Itoa(t1), strconv.Itoa(t2), strconv.Itoa(t3),
	}
	for i, w := range want {
		if m[i+1] != w {
			t.Errorf("README count group %d = %s, want %s (registry: %d total, %d/%d/%d/%d by tier)",
				i+1, m[i+1], w, total, t0, t1, t2, t3)
		}
	}
}

// TestResourceSurfaceInventory (phase10 §11 D5): the resource surface gets
// the same inventory discipline as the tool surface — exactly the four
// static kernel resources, and the grid template iff the grid is enabled.
func TestResourceSurfaceInventory(t *testing.T) {
	gt := newTestGT(t)
	probe := func(gridEnabled bool) (static, templates []string) {
		server := mcp.NewServer(&mcp.Implementation{Name: "res-drift", Version: "0"}, nil)
		registerKernelResources(server, gt.cfg, gt.st)
		uigrid.RegisterResource(server, uigrid.New(uigrid.RegistryOpts{Enabled: gridEnabled}), gt.aud)
		srvConn, cliConn := net.Pipe()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		go server.Run(ctx, &mcp.IOTransport{Reader: srvConn, Writer: srvConn})
		t.Cleanup(func() { cancel(); cliConn.Close() })
		sess, err := mcp.NewClient(&mcp.Implementation{Name: "res-drift-client", Version: "0"}, nil).
			Connect(ctx, &mcp.IOTransport{Reader: cliConn, Writer: cliConn}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		res, err := sess.ListResources(ctx, &mcp.ListResourcesParams{})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res.Resources {
			static = append(static, r.URI)
		}
		tpls, err := sess.ListResourceTemplates(ctx, &mcp.ListResourceTemplatesParams{})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range tpls.ResourceTemplates {
			templates = append(templates, r.URITemplate)
		}
		return static, templates
	}

	static, templates := probe(true)
	wantStatic := []string{"fbmcp://audit/head", "fbmcp://config", "fbmcp://policy", "fbmcp://registry"}
	if len(static) != len(wantStatic) {
		t.Fatalf("static resources = %v, want %v", static, wantStatic)
	}
	for i := range wantStatic {
		if static[i] != wantStatic[i] {
			t.Fatalf("static resources = %v, want %v", static, wantStatic)
		}
	}
	if len(templates) != 1 || templates[0] != uigrid.URIPrefix+"{result_id}" {
		t.Fatalf("templates = %v, want exactly [%s{result_id}]", templates, uigrid.URIPrefix)
	}
	_, templates = probe(false)
	if len(templates) != 0 {
		t.Fatalf("disabled grid must advertise no templates, got %v", templates)
	}
}

// TestKernelResourcesReadable (phase10 §11 D6): the four kernel resources
// get their first happy-path reads.
func TestKernelResourcesReadable(t *testing.T) {
	gt := newTestGT(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "res-read", Version: "0"}, nil)
	registerKernelResources(server, gt.cfg, gt.st)
	srvConn, cliConn := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go server.Run(ctx, &mcp.IOTransport{Reader: srvConn, Writer: srvConn})
	defer cliConn.Close()
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "res-read-client", Version: "0"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: cliConn, Writer: cliConn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	for _, uri := range []string{"fbmcp://registry", "fbmcp://policy", "fbmcp://config", "fbmcp://audit/head"} {
		rr, err := sess.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		if err != nil {
			t.Fatalf("%s: %v", uri, err)
		}
		if len(rr.Contents) != 1 || rr.Contents[0].MIMEType != "text/plain" || strings.TrimSpace(rr.Contents[0].Text) == "" {
			t.Fatalf("%s: contents %+v", uri, rr.Contents)
		}
	}
}
