// Package killharness hosts the P6.2 chaos-harness live tests (improvement
// plan WS1 A.1). Each scenario boots the real server binary against an
// isolated state dir, drives it over the attach socket with a minimal MCP
// client, kills it hard at a deterministic killpoint (internal/killpoint),
// restarts it, and checks the recovery invariants: audit chain verifies,
// state.json reloads, running jobs are marked interrupted, unconsumed
// pending actions replay (never silently dropped), and the single-kernel
// lock is reclaimed from the dead process.
//
// Gating: FBMCP_KILLHARNESS=1 enables the scenarios that need no Firebird
// (they exercise the gate/dispatch/persist pipeline through the demo tool);
// scenarios touching a real engine additionally require FBMCP_REQUIRE_FIREBIRD=1,
// matching the fuse #1 CI policy. Unset ⇒ skip, so plain `go test ./...`
// stays hermetic.
package killharness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/attach"
	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/state"
)

func requireHarness(t *testing.T) {
	t.Helper()
	if os.Getenv("FBMCP_KILLHARNESS") != "1" {
		t.Skip("set FBMCP_KILLHARNESS=1 to run the kill harness (builds and kills real server processes)")
	}
}

func requireFirebird(t *testing.T) {
	t.Helper()
	if os.Getenv("FBMCP_REQUIRE_FIREBIRD") != "1" {
		t.Skip("set FBMCP_REQUIRE_FIREBIRD=1 on the HQbird host to run Firebird-dependent scenarios")
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

func buildServer(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fbmcp-killharness.exe")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/fbmcp")
	cmd.Dir = moduleRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build server: %v\n%s", err, out)
	}
	return bin
}

// writeConfig derives a harness config from fbmcp.dev.yaml with the state
// dir redirected to an isolated directory.
func writeConfig(t *testing.T, stateDir string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(moduleRoot(t), "fbmcp.dev.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^(\s*dir:\s*).*$`)
	body := re.ReplaceAllString(string(src), "${1}"+filepath.ToSlash(stateDir))
	p := filepath.Join(t.TempDir(), "kill.yaml")
	if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

// startKernel boots the server binary. Stdin is a held-open pipe so the
// stdio transport never sees EOF while the harness talks over the attach
// socket. killpoints arms FBMCP_KILLPOINT ("" ⇒ none).
func startKernel(t *testing.T, bin, cfgPath, killpoints, kpDir string) (*exec.Cmd, *os.File) {
	t.Helper()
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-config", cfgPath)
	cmd.Stdin = stdinR
	var log bytes.Buffer
	cmd.Stdout = &log
	cmd.Stderr = &log
	env := append(os.Environ(), "FBMCP_DEV_PW=masterkey")
	if killpoints != "" {
		env = append(env, "FBMCP_KILLPOINT="+killpoints, "FBMCP_KILLPOINT_DIR="+kpDir)
	}
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		stdinW.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
		t.Logf("kernel output:\n%s", log.String())
	})
	return cmd, stdinW
}

// mcpClient is a minimal newline-delimited JSON-RPC client for the attach
// socket — just enough for initialize + tools/call.
type mcpClient struct {
	conn net.Conn
	dec  *bufio.Reader
	next int
}

func dialMCP(t *testing.T, stateDir string) *mcpClient {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var conn net.Conn
	var err error
	for {
		conn, err = attach.Dial(stateDir, 5*time.Second)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("attach to kernel: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	c := &mcpClient{conn: conn, dec: bufio.NewReader(conn), next: 1}
	t.Cleanup(func() { c.conn.Close() })
	c.roundTrip(t, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "killharness", "version": "0"},
	})
	c.notify("notifications/initialized")
	return c
}

func (c *mcpClient) send(t *testing.T, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.conn.Write(append(b, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func (c *mcpClient) notify(method string) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	c.conn.Write(append(b, '\n'))
}

func (c *mcpClient) roundTrip(t *testing.T, method string, params any) map[string]any {
	t.Helper()
	id := c.next
	c.next++
	c.send(t, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	for {
		line, err := c.dec.ReadString('\n')
		if err != nil {
			t.Fatalf("read response for %s: %v", method, err)
		}
		var msg map[string]any
		if json.Unmarshal([]byte(line), &msg) != nil {
			continue
		}
		if fmt.Sprint(msg["id"]) != fmt.Sprint(id) {
			continue // server notification or request on another id
		}
		if e, ok := msg["error"].(map[string]any); ok {
			t.Fatalf("%s rpc error: %v", method, e)
		}
		res, _ := msg["result"].(map[string]any)
		if res == nil {
			t.Fatalf("%s: missing result: %s", method, line)
		}
		return res
	}
}

func (c *mcpClient) callTool(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	res := c.roundTrip(t, "tools/call", map[string]any{"name": name, "arguments": args})
	content, _ := res["content"].([]any)
	for _, it := range content {
		if m, ok := it.(map[string]any); ok {
			if txt, ok := m["text"].(string); ok {
				return txt
			}
		}
	}
	t.Fatalf("tools/call %s: no text content in %v", name, res)
	return ""
}

// tryCallTool is callTool without test fatalization, for calls whose
// response may legitimately never arrive (the kernel is killed mid-call).
func (c *mcpClient) tryCallTool(name string, args map[string]any) (string, error) {
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.next, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if err != nil {
		return "", err
	}
	id := c.next
	c.next++
	if _, err := c.conn.Write(append(b, '\n')); err != nil {
		return "", err
	}
	for {
		line, err := c.dec.ReadString('\n')
		if err != nil {
			return "", err
		}
		var msg map[string]any
		if json.Unmarshal([]byte(line), &msg) != nil {
			continue
		}
		if fmt.Sprint(msg["id"]) != fmt.Sprint(id) {
			continue
		}
		if e, ok := msg["error"].(map[string]any); ok {
			return "", fmt.Errorf("%s rpc error: %v", name, e)
		}
		res, _ := msg["result"].(map[string]any)
		content, _ := res["content"].([]any)
		for _, it := range content {
			if m, ok := it.(map[string]any); ok {
				if txt, ok := m["text"].(string); ok {
					return txt, nil
				}
			}
		}
		return "", fmt.Errorf("no text content in %v", res)
	}
}

func waitReady(t *testing.T, kpDir, name string, timeout time.Duration) {
	t.Helper()
	p := filepath.Join(kpDir, name+".ready")
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(p); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("killpoint %s never became ready", name)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func mustFind(t *testing.T, s, pattern string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(s)
	if len(m) != 2 {
		t.Fatalf("pattern %s not found in %q", pattern, s)
	}
	return m[1]
}

// restartAndVerify boots a fresh kernel on the same state dir and checks the
// post-kill invariants. jobID ("" = none) must end up interrupted; pendingIDs
// lists request ids that must still be pending after the restart.
func restartAndVerify(t *testing.T, bin, cfgPath, stateDir, jobID string, pendingIDs ...string) {
	t.Helper()
	cmd, keep := startKernel(t, bin, cfgPath, "", "")
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
		keep.Close()
	}()
	c := dialMCP(t, stateDir)
	if got := c.callTool(t, "fb_ping", map[string]any{}); !strings.Contains(got, "pong") {
		t.Fatalf("restarted kernel unhealthy: %q", got)
	}
	if _, err := audit.Verify(filepath.Join(stateDir, "audit.jsonl")); err != nil {
		t.Fatalf("audit chain broken after kill (C5): %v", err)
	}
	if jobID != "" {
		deadline := time.Now().Add(20 * time.Second)
		for {
			st, err := state.Open(stateDir)
			if err != nil {
				t.Fatalf("state.json unreadable after kill: %v", err)
			}
			for _, j := range st.Jobs() {
				if j.ID != jobID {
					continue
				}
				switch j.State {
				case "interrupted":
					return // reconcile marked it; invariants hold
				case "succeeded", "failed":
					t.Fatalf("job %s recorded %s despite the kill — possible double-dispatch", jobID, j.State)
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("job %s not marked interrupted after restart", jobID)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	if len(pendingIDs) > 0 {
		st, err := state.Open(stateDir)
		if err != nil {
			t.Fatalf("state.json unreadable after kill: %v", err)
		}
		have := map[string]bool{}
		for _, p := range st.Pending() {
			have[p.ID] = true
		}
		for _, id := range pendingIDs {
			if !have[id] {
				t.Fatalf("pending action %s silently dropped across the kill (replay invariant)", id)
			}
		}
	}
}

// runKillScenario drives tool on spike5 through the full gate pipeline,
// hard-kills the kernel at the armed killpoint, then verifies recovery.
// The confirm response returns before the job killpoint blocks (the job body
// runs on the worker goroutine), so the flow can read it synchronously.
func runKillScenario(t *testing.T, killpoint, tool string) {
	t.Helper()
	bin := buildServer(t)
	stateDir := t.TempDir()
	kpDir := filepath.Join(stateDir, "kp")
	cfg := writeConfig(t, stateDir)

	cmd, keep := startKernel(t, bin, cfg, killpoint, kpDir)
	defer keep.Close()
	c := dialMCP(t, stateDir)

	out := c.callTool(t, tool, map[string]any{"db": "spike5"})
	requestID := mustFind(t, out, `Request ID: ([0-9a-f]+)`)
	tok := mustFind(t, out, `token \(Tier 1 only\): ([0-9a-f]+)`)
	cout := c.callTool(t, "fb_confirm", map[string]any{"request_id": requestID, "token": tok})
	jobID := mustFind(t, cout, `job ([a-z0-9]+)`)

	waitReady(t, kpDir, killpoint, 90*time.Second)
	if err := cmd.Process.Kill(); err != nil { // hard kill: the Windows kill -9
		t.Fatal(err)
	}
	cmd.Wait()

	restartAndVerify(t, bin, cfg, stateDir, jobID)
}

// runKillPendingScenario kills while the gate.Request call is still blocked
// at the killpoint — before the impact statement (and request id) ever
// reaches the client, and before any confirmation. The unconsumed pending
// action must replay across the restart.
func runKillPendingScenario(t *testing.T) {
	t.Helper()
	bin := buildServer(t)
	stateDir := t.TempDir()
	kpDir := filepath.Join(stateDir, "kp")
	cfg := writeConfig(t, stateDir)

	cmd, keep := startKernel(t, bin, cfg, "gate.pending", kpDir)
	defer keep.Close()
	c := dialMCP(t, stateDir)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.tryCallTool("fb_demo_write", map[string]any{"db": "spike5"}) // response never arrives
	}()

	waitReady(t, kpDir, "gate.pending", 90*time.Second)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()

	restartAndVerify(t, bin, cfg, stateDir, "")
	st, err := state.Open(stateDir)
	if err != nil {
		t.Fatalf("state.json unreadable after kill: %v", err)
	}
	for _, p := range st.Pending() {
		if p.Tool == "fb_demo_write" {
			return // replayed, not dropped
		}
	}
	t.Fatalf("fb_demo_write pending action lost across the kill (replay invariant): %+v", st.Pending())
}

func TestKillAtGatePending(t *testing.T) {
	requireHarness(t)
	runKillPendingScenario(t)
}

func TestKillAtJobRunning(t *testing.T) {
	requireHarness(t)
	runKillScenario(t, "job.running", "fb_demo_write")
}

func TestKillAtBackupStarted(t *testing.T) {
	requireHarness(t)
	requireFirebird(t)
	runKillScenario(t, "backup.started", "fb_backup_start")
}
