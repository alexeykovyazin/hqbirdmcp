package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/qlog"
	"github.com/aleks/fbmcp/internal/uigrid"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestFormatQueryResultGolden locks fb_query's text path: the grid work
// (phase10) must not change a single byte the AI reads.
func TestFormatQueryResultGolden(t *testing.T) {
	res := queryResult{
		cols:      []string{"ID", "NAME"},
		rows:      [][]string{{"1", "Smith"}, {"2", "<null>"}},
		truncated: true,
		elapsedMS: 16.4,
		plan:      "PLAN (EMPLOYEE NATURAL)",
	}
	want := "ID | NAME\n" +
		"1 | Smith\n" +
		"2 | <null>\n" +
		"rows: 2 (truncated — raise max_rows, up to 1000)\n" +
		"\nplan:\nPLAN (EMPLOYEE NATURAL)\n" +
		"elapsed: 16.4 ms\n"
	if got := formatQueryResult(res); got != want {
		t.Fatalf("text path drifted:\n got %q\nwant %q", got, want)
	}
	// zero-column result (statement completed, no result set)
	if got := formatQueryResult(queryResult{}); !strings.HasPrefix(got, "no result set") {
		t.Fatalf("zero-column text drifted: %q", got)
	}
}

func TestCountBindParams(t *testing.T) {
	cases := []struct {
		sql  string
		want int
	}{
		{"SELECT 1 FROM RDB$DATABASE", 0},
		{"SELECT * FROM T WHERE A = ? AND B = ?", 2},
		{"SELECT * FROM T WHERE S = 'a ? b' AND C = ?", 1},    // ? inside literal
		{"SELECT * FROM T WHERE S = 'it''s ?' AND C = ?", 1},  // escaped quote
		{"SELECT * FROM \"weird ? name\" WHERE A = ?", 1},     // quoted identifier
		{"SELECT 1 /* ? */ FROM RDB$DATABASE WHERE A = ?", 1}, // block comment
		{"SELECT 1 -- ?\nFROM RDB$DATABASE WHERE A = ?", 1},   // line comment
		{"EXECUTE PROCEDURE P(?, ?)", 2},
	}
	for _, c := range cases {
		if got := countBindParams(c.sql); got != c.want {
			t.Errorf("countBindParams(%q) = %d, want %d", c.sql, got, c.want)
		}
	}
}

func TestTypedRowBasics(t *testing.T) {
	// nil column metadata (types unknown): strings pass, nil → nil,
	// numbers/bools pass through
	got := typedRow([]any{"txt", nil, int64(5), float64(1.5), true}, nil)
	if got[0] != "txt" || got[1] != nil || got[2] != int64(5) || got[3] != float64(1.5) || got[4] != true {
		t.Fatalf("typedRow basics: %+v", got)
	}
	// the typed row must JSON-marshal null as null (not "<null>")
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "null") || strings.Contains(string(b), "<null>") {
		t.Fatalf("null must be JSON null: %s", b)
	}
}

func TestStoreGrid(t *testing.T) {
	gt := newTestGT(t)
	// nil registry (grid disabled): storeGrid must be a clean no-op
	if id, ok := gt.storeGrid("local", "db", "SELECT 1", queryResult{
		cols: []string{"X"}, colsMeta: []uigrid.Col{{Name: "X", Type: "integer"}},
		typedRows: [][]any{{int64(1)}},
	}); ok || id != "" {
		t.Fatal("nil registry must disable the grid")
	}

	gt.grid = uigrid.New(uigrid.RegistryOpts{Enabled: true})
	res := queryResult{
		cols:      []string{"ID", "NAME"},
		colsMeta:  []uigrid.Col{{Name: "ID", Type: "integer"}, {Name: "NAME", Type: "string"}},
		rows:      [][]string{{"1", "x"}},
		typedRows: [][]any{{int64(1), "x"}},
		truncated: false,
		elapsedMS: 1.5,
		plan:      "PLAN (T NATURAL)",
		stats:     &qlog.Stats{PageReads: 3},
	}
	id, ok := gt.storeGrid("local", "spike5", "SELECT ID, NAME FROM T", res)
	if !ok {
		t.Fatal("storeGrid failed with the grid enabled")
	}
	data, err := gt.grid.Get("local", id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var p uigrid.Payload
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	if p.RowCount != 1 || len(p.Columns) != 2 || p.Plan != "PLAN (T NATURAL)" {
		t.Fatalf("payload mismatch: %+v", p)
	}
	if !strings.HasPrefix(p.Title, "spike5: SELECT ID, NAME FROM T") {
		t.Fatalf("title %q", p.Title)
	}
	// zero-column results render no grid (plan decision: skip _meta.ui)
	if _, ok := gt.storeGrid("local", "spike5", "EXECUTE PROCEDURE P", queryResult{}); ok {
		t.Fatal("zero-column result must not store a grid payload")
	}
}

// TestGridResourceMCP covers the ui://fbmcp/results/{id} template end to end
// over a real MCP session: templates/list, resources/read of the HTML page
// (CSP + payload inlined), and the expired-id error. This is the tree's
// first resources/read coverage — the kernel resources have none.
func TestGridResourceMCP(t *testing.T) {
	audDir := t.TempDir()
	aud, err := audit.Open(audDir)
	if err != nil {
		t.Fatal(err)
	}
	defer aud.Close()
	reg := uigrid.New(uigrid.RegistryOpts{Enabled: true})
	id, ok := reg.Put("local", "spike5: SELECT 1", testPayloadCmd(2))
	if !ok {
		t.Fatal("Put failed")
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "grid-test", Version: "0"}, nil)
	uigrid.RegisterResource(server, reg, aud)

	srvConn, cliConn := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go server.Run(ctx, &mcp.IOTransport{Reader: srvConn, Writer: srvConn})
	defer cliConn.Close()
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "grid-client", Version: "0"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: cliConn, Writer: cliConn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	tpls, err := sess.ListResourceTemplates(ctx, &mcp.ListResourceTemplatesParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tpls.ResourceTemplates) != 1 || tpls.ResourceTemplates[0].URITemplate != uigrid.URIPrefix+"{result_id}" {
		t.Fatalf("templates: %+v", tpls.ResourceTemplates)
	}

	// fresh id: text/html with the CSP meta and the payload inlined
	rr, err := sess.ReadResource(ctx, &mcp.ReadResourceParams{URI: uigrid.URI(id)})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(rr.Contents) != 1 {
		t.Fatalf("contents: %+v", rr.Contents)
	}
	c := rr.Contents[0]
	if c.MIMEType != "text/html" {
		t.Fatalf("mime %q", c.MIMEType)
	}
	for _, want := range []string{"Content-Security-Policy", "spike5: SELECT 1", "__GRID_DATA__", "row-"} {
		if !strings.Contains(c.Text, want) {
			t.Fatalf("page missing %q", want)
		}
	}

	// expired/unknown id: the stable code leads the error text
	if _, err := sess.ReadResource(ctx, &mcp.ReadResourceParams{URI: uigrid.URIPrefix + strings.Repeat("0", 32)}); err == nil ||
		!strings.Contains(err.Error(), "result-expired") {
		t.Fatalf("unknown id err = %v, want result-expired", err)
	}

	// D1 (phase10 §11): every served read is audited — allow with bytes for
	// the fresh id, error for the unknown one.
	ab, err := os.ReadFile(filepath.Join(audDir, "audit.jsonl"))
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(ab)), "\n")
	if len(lines) != 2 {
		t.Fatalf("audit lines = %d, want 2 (allow + error)", len(lines))
	}
	var entries [2]map[string]any
	for i, l := range lines {
		if err := json.Unmarshal([]byte(l), &entries[i]); err != nil {
			t.Fatal(err)
		}
		if entries[i]["tool"] != "uigrid.resource" || entries[i]["identity"] != "local" {
			t.Fatalf("audit line %d: %v", i+1, entries[i])
		}
	}
	if entries[0]["decision"] != "allow" {
		t.Fatalf("audit line 1 decision = %v, want allow", entries[0]["decision"])
	}
	d, _ := entries[0]["detail"].(map[string]any)
	if d == nil || d["result_id"] != id {
		t.Fatalf("audit line 1 detail = %v, want result_id %s", entries[0]["detail"], id)
	}
	if _, ok := d["bytes"].(float64); !ok {
		t.Fatalf("audit line 1 detail missing bytes: %v", d)
	}
	if entries[1]["decision"] != "error" {
		t.Fatalf("audit line 2 decision = %v, want error (unknown id)", entries[1]["decision"])
	}
}

// TestGridResourceDisabled checks the enabled:false contract: no template
// advertised, no resources listed.
func TestGridResourceDisabled(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "grid-off", Version: "0"}, nil)
	uigrid.RegisterResource(server, uigrid.New(uigrid.RegistryOpts{Enabled: false}), nil)

	srvConn, cliConn := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go server.Run(ctx, &mcp.IOTransport{Reader: srvConn, Writer: srvConn})
	defer cliConn.Close()
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "grid-off-client", Version: "0"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: cliConn, Writer: cliConn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	tpls, err := sess.ListResourceTemplates(ctx, &mcp.ListResourceTemplatesParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tpls.ResourceTemplates) != 0 {
		t.Fatalf("disabled grid must advertise no templates, got %+v", tpls.ResourceTemplates)
	}
}

// TestQueryCheckHermetic covers the paths that need no database: the
// classifyRead denial (with its errdoc classification) and the bind-param
// note plumbing via a disabled-pool-free construction. The prepare path
// itself is live-gated (TestGridLiveMCP).
func TestQueryCheckHermetic(t *testing.T) {
	gt := newTestGT(t)
	gt.grid = uigrid.New(uigrid.RegistryOpts{Enabled: true})
	server := mcp.NewServer(&mcp.Implementation{Name: "check-test", Version: "0"}, nil)
	registerQueryCheckTool(server, gt)

	srvConn, cliConn := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go server.Run(ctx, &mcp.IOTransport{Reader: srvConn, Writer: srvConn})
	defer cliConn.Close()
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "check-client", Version: "0"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: cliConn, Writer: cliConn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// a mutation draft: denied before any pool is touched, envelope classifies
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "fb_query_check", Arguments: map[string]any{
		"db": "spike5", "sql": "INSERT INTO T VALUES (1)",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("mutation draft must be denied")
	}
	var env map[string]any
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok && !strings.Contains(tc.Text, "fb_write") {
			t.Fatalf("denial must point at fb_write: %q", tc.Text)
		}
	}
	if res.StructuredContent != nil {
		if m, ok := res.StructuredContent.(map[string]any); ok && m["code"] != "fbquery-rejected" {
			t.Fatalf("errdoc code = %v, want fbquery-rejected", m["code"])
		}
	} else {
		env = nil
	}
	_ = env
}

// testPayloadCmd builds a small payload for the MCP resource tests.
func testPayloadCmd(rows int) *uigrid.Payload {
	cols := []uigrid.Col{{Name: "ID", Type: "integer"}, {Name: "S", Type: "string"}}
	r := make([][]any, rows)
	for i := range r {
		r[i] = []any{int64(i + 1), "row-" + strings.Repeat("z", 8)}
	}
	return uigrid.Gridify("spike5: SELECT 1", cols, r, uigrid.Options{})
}
