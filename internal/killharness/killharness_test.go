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
	"crypto/sha256"
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
	// The C7 scenarios assert strict byte-level source-database invariants;
	// the C.4 trends sampler attaches to every configured database on its
	// ticker, and even a read-only-transaction attachment makes the engine
	// touch the file (housekeeping on attach/detach). Disable it here so the
	// invariants measure the killed workflow, not sampler side effects.
	body += "\ntrends:\n    disabled: true\n"

	// Per-run database COPIES: the scenarios share the dev spike files with
	// anything else running on the host (the soak kernel samples them every
	// 5 minutes; its attachments change bytes and broke the C7a invariants
	// — observed live 2026-08-31). Redirect every database path to a copy
	// under the isolated state dir.
	dbDir := filepath.Join(stateDir, "dbs")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pathRe := regexp.MustCompile(`(?m)^(\s*path:\s*)(.+)$`)
	copies := map[string]string{}
	for _, m := range pathRe.FindAllStringSubmatch(string(src), -1) {
		orig := strings.TrimSpace(m[2])
		if _, done := copies[orig]; done {
			continue
		}
		b, err := os.ReadFile(filepath.FromSlash(strings.TrimSpace(orig)))
		if err != nil {
			continue // not a local file / not copyable — leave the path as-is
		}
		dst := filepath.ToSlash(filepath.Join(dbDir, filepath.Base(strings.TrimSpace(orig))))
		if err := os.WriteFile(filepath.FromSlash(dst), b, 0o644); err != nil {
			t.Fatal(err)
		}
		copies[orig] = dst
	}
	body = pathRe.ReplaceAllStringFunc(body, func(line string) string {
		m := pathRe.FindStringSubmatch(line)
		if c, ok := copies[strings.TrimSpace(m[2])]; ok {
			return m[1] + c
		}
		return line
	})
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

// confirmTool runs a Tier-1 tool through request → in-band confirm and
// returns the submitted job id.
func confirmTool(t *testing.T, c *mcpClient, tool string, args map[string]any) string {
	t.Helper()
	out := c.callTool(t, tool, args)
	rid := mustFind(t, out, `Request ID: ([0-9a-f]+)`)
	tok := mustFind(t, out, `token \(Tier 1 only\): ([0-9a-f]+)`)
	cout := c.callTool(t, "fb_confirm", map[string]any{"request_id": rid, "token": tok})
	return mustFind(t, cout, `job ([a-z0-9]+)`)
}

func waitJob(t *testing.T, c *mcpClient, jobID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		out := c.callTool(t, "fb_job_status", map[string]any{"job_id": jobID})
		if strings.Contains(out, "succeeded") {
			return
		}
		if strings.Contains(out, "failed") || strings.Contains(out, "interrupted") {
			t.Fatalf("job %s terminal: %s", jobID, out)
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s timeout: %s", jobID, out)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// TestKillAtRestoreReplaceC7a is the claims-register C7a scenario: kill the
// kernel during a Tier-2 restore_replace (approved out-of-band, exactly as
// the channel policy demands) at the wf.replace checkpoint — after the
// database file is removed and the .pre-restore snapshot exists, before the
// restore runs. Invariants: the original file is intact or snapshotted;
// after restart the AutoReopen reconciliation drives the database back
// online; the audit chain verifies.
func TestKillAtRestoreReplaceC7a(t *testing.T) {
	requireHarness(t)
	requireFirebird(t)
	bin := buildServer(t)
	stateDir := t.TempDir()
	kpDir := filepath.Join(stateDir, "kp")
	cfg := writeConfig(t, stateDir)

	src, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := mustFind(t, string(src), `(?s)id: spike5.*?path: (\S+)`)

	cmd, keep := startKernel(t, bin, cfg, "wf.replace", kpDir)
	defer keep.Close()
	c := dialMCP(t, stateDir)

	// fresh verified backup: restore_replace preconditions (verified_backup_exists, < 24h)
	waitJob(t, c, confirmTool(t, c, "fb_backup_start", map[string]any{"db": "spike5"}), 180*time.Second)
	waitJob(t, c, confirmTool(t, c, "fb_restore_test", map[string]any{"db": "spike5"}), 180*time.Second)
	// Tier-2 tools require an open maintenance window
	waitJob(t, c, confirmTool(t, c, "fb_window_open", map[string]any{"db": "spike5", "args": map[string]any{"hours": 1}}), 60*time.Second)

	// Tier 2: out-of-band approval only — drop the marker the watcher polls
	out := c.callTool(t, "fb_restore_replace", map[string]any{"db": "spike5"})
	rid := mustFind(t, out, `Request ID: ([0-9a-f]+)`)
	if strings.Contains(out, "In-band token") {
		t.Fatalf("Tier 2 action offered an in-band token — channel policy violated: %q", out)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "approvals"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "approvals", rid), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	waitReady(t, kpDir, "wf.replace", 120*time.Second)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()

	// C7a kill-state invariant: file intact or .pre-restore present
	pre := dbPath + ".pre-restore"
	if _, err := os.Stat(pre); err != nil {
		if _, err := os.Stat(dbPath); err != nil {
			t.Fatalf("neither %s nor %s exists after the kill — data-loss state", dbPath, pre)
		}
	}

	// restart: reconcile must finish the AutoReopen workflow and bring the DB online
	cmd2, keep2 := startKernel(t, bin, cfg, "", "")
	defer func() {
		cmd2.Process.Kill()
		cmd2.Wait()
		keep2.Close()
	}()
	c2 := dialMCP(t, stateDir)
	deadline := time.Now().Add(180 * time.Second)
	for {
		got := c2.callTool(t, "fb_db_health", map[string]any{"db": "spike5"})
		if strings.Contains(got, "online") {
			break
		}
		if time.Now().After(deadline) {
			if b, err := os.ReadFile(filepath.Join(stateDir, "state.json")); err == nil {
				t.Logf("state.json at timeout:\n%s", b)
			} else {
				t.Logf("state.json unreadable: %v", err)
			}
			if _, err := os.Stat(dbPath); err != nil {
				t.Logf("db file %s missing: %v", dbPath, err)
			}
			t.Fatalf("spike5 never came back online after reconcile: %q", got)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if _, err := audit.Verify(filepath.Join(stateDir, "audit.jsonl")); err != nil {
		t.Fatalf("audit chain broken after kill (C5): %v", err)
	}
}

// runShutdownKillScenario is the C7a shutdown_window variant: Tier-2
// shutdown (window + verified backup preconditions, OOB approval), killed at
// the given checkpoint (wf.shut = database shut but not yet online;
// db.closedb = mid pool close, the P6.2 T6 / P3 finding #3 case). The file
// must stay intact and the restart must bring the database back online.
func runShutdownKillScenario(t *testing.T, killpointName string) {
	t.Helper()
	bin := buildServer(t)
	stateDir := t.TempDir()
	kpDir := filepath.Join(stateDir, "kp")
	cfg := writeConfig(t, stateDir)

	src, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := mustFind(t, string(src), `(?s)id: spike5.*?path: (\S+)`)

	cmd, keep := startKernel(t, bin, cfg, killpointName, kpDir)
	defer keep.Close()
	c := dialMCP(t, stateDir)

	waitJob(t, c, confirmTool(t, c, "fb_backup_start", map[string]any{"db": "spike5"}), 180*time.Second)
	waitJob(t, c, confirmTool(t, c, "fb_restore_test", map[string]any{"db": "spike5"}), 180*time.Second)
	waitJob(t, c, confirmTool(t, c, "fb_window_open", map[string]any{"db": "spike5", "args": map[string]any{"hours": 1}}), 60*time.Second)

	out := c.callTool(t, "fb_shutdown_window", map[string]any{"db": "spike5"})
	rid := mustFind(t, out, `Request ID: ([0-9a-f]+)`)
	if strings.Contains(out, "In-band token") {
		t.Fatalf("Tier 2 action offered an in-band token — channel policy violated: %q", out)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "approvals"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "approvals", rid), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	waitReady(t, kpDir, killpointName, 120*time.Second)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()

	// C7a kill-state invariant: shutdown never removes the file
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database file %s missing after shutdown kill: %v", dbPath, err)
	}

	// restart: AutoReopen must finish the workflow tail (gfix -online)
	cmd2, keep2 := startKernel(t, bin, cfg, "", "")
	defer func() {
		cmd2.Process.Kill()
		cmd2.Wait()
		keep2.Close()
	}()
	c2 := dialMCP(t, stateDir)
	deadline := time.Now().Add(180 * time.Second)
	for {
		got := c2.callTool(t, "fb_db_health", map[string]any{"db": "spike5"})
		if strings.Contains(got, "online") {
			break
		}
		if time.Now().After(deadline) {
			if b, err := os.ReadFile(filepath.Join(stateDir, "state.json")); err == nil {
				t.Logf("state.json at timeout:\n%s", b)
			}
			t.Fatalf("spike5 never came back online after shutdown kill: %q", got)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if _, err := audit.Verify(filepath.Join(stateDir, "audit.jsonl")); err != nil {
		t.Fatalf("audit chain broken after kill (C5): %v", err)
	}
}

func TestKillAtShutdownWindowC7a(t *testing.T) {
	requireHarness(t)
	requireFirebird(t)
	runShutdownKillScenario(t, "wf.shut")
}

func TestKillDuringCloseDB(t *testing.T) {
	requireHarness(t)
	requireFirebird(t)
	runShutdownKillScenario(t, "db.closedb")
}

// TestKillNightlyVerifyC7b is the claims-register C7b scenario: a durable
// schedule grant (target nightly_verify, fires every minute) starts the
// backup→restore-test chain, and the kernel is hard-killed mid-backup. The
// source database file must be byte-identical, the schedule grant must
// survive the restart, and the audit chain must verify.
func TestKillNightlyVerifyC7b(t *testing.T) {
	requireHarness(t)
	requireFirebird(t)
	bin := buildServer(t)
	stateDir := t.TempDir()
	kpDir := filepath.Join(stateDir, "kp")
	cfg := writeConfig(t, stateDir)

	src, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := mustFind(t, string(src), `(?s)id: spike5.*?path: (\S+)`)
	before := fileHash(t, dbPath)

	cmd, keep := startKernel(t, bin, cfg, "backup.started", kpDir)
	defer keep.Close()
	c := dialMCP(t, stateDir)

	out := c.callTool(t, "fb_schedule_create", map[string]any{
		"db": "spike5", "target": "nightly_verify", "cron": "* * * * *", "timezone": "UTC",
	})
	rid := mustFind(t, out, `Request ID: ([0-9a-f]+)`)
	tok := mustFind(t, out, `token \(Tier 1 only\): ([0-9a-f]+)`)
	cout := c.callTool(t, "fb_confirm", map[string]any{"request_id": rid, "token": tok})
	if !strings.Contains(cout, "confirmed") {
		t.Fatalf("schedule confirm failed: %q", cout)
	}

	// next minute boundary + dispatch + service attach
	waitReady(t, kpDir, "backup.started", 150*time.Second)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()

	if after := fileHash(t, dbPath); after != before {
		t.Fatalf("source database modified by killed nightly_verify (C7b): %s != %s", before, after)
	}

	cmd2, keep2 := startKernel(t, bin, cfg, "", "")
	defer func() {
		cmd2.Process.Kill()
		cmd2.Wait()
		keep2.Close()
	}()
	c2 := dialMCP(t, stateDir)
	if got := c2.callTool(t, "fb_schedule_list", map[string]any{"db": "spike5"}); !strings.Contains(got, "nightly_verify") {
		t.Fatalf("schedule grant lost across the kill: %q", got)
	}
	if _, err := audit.Verify(filepath.Join(stateDir, "audit.jsonl")); err != nil {
		t.Fatalf("audit chain broken after kill (C5): %v", err)
	}
	if after := fileHash(t, dbPath); after != before {
		t.Fatalf("source database modified after restart (C7b): %s != %s", before, after)
	}
}

func fileHash(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

// TestCorruptAuditTailRefusesStart is the P6.2 T1 audit-corruption
// injection: with the audit chain's last line truncated, the kernel must
// refuse to start (head-sidecar mismatch, fail-closed) — never append onto
// a broken chain — and audit.Verify must detect the damage.
func TestCorruptAuditTailRefusesStart(t *testing.T) {
	requireHarness(t)
	bin := buildServer(t)
	stateDir := t.TempDir()
	cfg := writeConfig(t, stateDir)

	cmd, keep := startKernel(t, bin, cfg, "", "")
	c := dialMCP(t, stateDir)
	out, err := c.tryCallTool("fb_demo_write", map[string]any{"db": "spike5"})
	if err != nil || !strings.Contains(out, "Request ID") {
		t.Fatalf("demo write did not reach the gate: %v %q", err, out)
	}
	cmd.Process.Kill()
	cmd.Wait()
	keep.Close()

	auditPath := filepath.Join(stateDir, "audit.jsonl")
	b, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 1 {
		t.Fatalf("expected at least 1 audit line, got %d", len(lines))
	}
	lines[len(lines)-1] = lines[len(lines)-1][:len(lines[len(lines)-1])/2]
	if err := os.WriteFile(auditPath, []byte(strings.Join(lines, "\n")+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.Verify(auditPath); err == nil {
		t.Fatal("audit.Verify did not detect the corrupted tail")
	}

	cmd2, keep2 := startKernel(t, bin, cfg, "", "")
	defer keep2.Close()
	exited := make(chan error, 1)
	go func() { exited <- cmd2.Wait() }()
	select {
	case <-exited: // fail-closed: refused to start on a broken chain
	case <-time.After(30 * time.Second):
		cmd2.Process.Kill()
		t.Fatal("kernel did not refuse to start on a corrupted audit tail")
	}
}

// TestDeadWebhookIsNonFatal is the P6.2 T1 dead-webhook injection: with the
// K7 webhook pointing at an unreachable endpoint, gated jobs must still run
// to completion and the kernel must stay healthy.
func TestDeadWebhookIsNonFatal(t *testing.T) {
	requireHarness(t)
	bin := buildServer(t)
	stateDir := t.TempDir()
	cfg := writeConfig(t, stateDir)

	src, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dead := string(src)
	if strings.Contains(dead, "webhook_url:") {
		dead = regexp.MustCompile(`(?m)^(\s*webhook_url:\s*).*$`).ReplaceAllString(dead, "${1}http://127.0.0.1:9/hook")
	} else {
		dead = strings.Replace(dead, "\nnotify:\n", "\nnotify:\n    webhook_url: http://127.0.0.1:9/hook\n", 1)
	}
	deadCfg := filepath.Join(t.TempDir(), "dead-webhook.yaml")
	if err := os.WriteFile(deadCfg, []byte(dead), 0o640); err != nil {
		t.Fatal(err)
	}

	_, keep := startKernel(t, bin, deadCfg, "", "")
	defer keep.Close()
	c := dialMCP(t, stateDir)

	jobID := confirmTool(t, c, "fb_demo_write", map[string]any{"db": "spike5"})
	waitJob(t, c, jobID, 60*time.Second)

	time.Sleep(5 * time.Second) // failed deliveries get retry time
	if got := c.callTool(t, "fb_ping", map[string]any{}); !strings.Contains(got, "pong") {
		t.Fatalf("kernel unhealthy after dead-webhook emissions: %q", got)
	}
	if _, err := audit.Verify(filepath.Join(stateDir, "audit.jsonl")); err != nil {
		t.Fatalf("audit chain broken by dead-webhook emissions: %v", err)
	}
}

// TestKillAtStateMidPersist kills between the durable temp write and the
// atomic rename (the state.mid-persist checkpoint inside Store.persist).
// Invariant: state.json loads cleanly after the kill — the old or the new
// snapshot, never torn — and the restarted kernel is healthy.
func TestKillAtStateMidPersist(t *testing.T) {
	requireHarness(t)
	bin := buildServer(t)
	stateDir := t.TempDir()
	kpDir := filepath.Join(stateDir, "kp")
	cfg := writeConfig(t, stateDir)

	cmd, keep := startKernel(t, bin, cfg, "state.mid-persist", kpDir)
	defer keep.Close()
	c := dialMCP(t, stateDir)

	// any state mutation reaches the checkpoint; the response is never sent
	go func() { _, _ = c.tryCallTool("fb_demo_write", map[string]any{"db": "spike5"}) }()
	waitReady(t, kpDir, "state.mid-persist", 60*time.Second)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()

	if _, err := state.Open(stateDir); err != nil {
		t.Fatalf("state.json torn after mid-persist kill (atomicity violated): %v", err)
	}

	cmd2, keep2 := startKernel(t, bin, cfg, "", "")
	defer func() {
		cmd2.Process.Kill()
		cmd2.Wait()
		keep2.Close()
	}()
	c2 := dialMCP(t, stateDir)
	if got := c2.callTool(t, "fb_ping", map[string]any{}); !strings.Contains(got, "pong") {
		t.Fatalf("kernel unhealthy after mid-persist kill: %q", got)
	}
	if _, err := audit.Verify(filepath.Join(stateDir, "audit.jsonl")); err != nil {
		t.Fatalf("audit chain broken after kill (C5): %v", err)
	}
}

// TestRestoreTestNoMatviewsLive is a non-kill live scenario (phase8_plan
// D2.2): fb_restore_test with args.no_matviews must complete through the
// gbak -SE -NO_MATVIEWS fallback and mark the catalog verified.
func TestRestoreTestNoMatviewsLive(t *testing.T) {
	requireHarness(t)
	requireFirebird(t)
	bin := buildServer(t)
	stateDir := t.TempDir()
	cfg := writeConfig(t, stateDir)

	cmd, keep := startKernel(t, bin, cfg, "", "")
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
		keep.Close()
	}()
	c := dialMCP(t, stateDir)

	waitJob(t, c, confirmTool(t, c, "fb_backup_start", map[string]any{"db": "spike5"}), 180*time.Second)
	waitJob(t, c, confirmTool(t, c, "fb_restore_test", map[string]any{"db": "spike5", "args": map[string]any{"no_matviews": true}}), 180*time.Second)
}

// TestSurfacesAndWaitLive verifies D3.2/D3.3 live: prompts/list and
// resources/list are served, a resource reads, fb_job_status returns
// structuredContent, and fb_confirm with wait returns the terminal state.
func TestSurfacesAndWaitLive(t *testing.T) {
	requireHarness(t)
	bin := buildServer(t)
	stateDir := t.TempDir()
	cfg := writeConfig(t, stateDir)

	cmd, keep := startKernel(t, bin, cfg, "", "")
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
		keep.Close()
	}()
	c := dialMCP(t, stateDir)

	// prompts/list exposes the playbooks (prompts/ is the source of truth)
	res := c.roundTrip(t, "prompts/list", map[string]any{})
	names, _ := res["prompts"].([]any)
	if len(names) < 5 {
		t.Fatalf("expected the six playbooks in prompts/list, got %v", names)
	}

	// resources/list + one read
	res = c.roundTrip(t, "resources/list", map[string]any{})
	uris, _ := res["resources"].([]any)
	if len(uris) < 4 {
		t.Fatalf("expected four kernel resources, got %v", uris)
	}
	read := c.roundTrip(t, "resources/read", map[string]any{"uri": "fbmcp://policy"})
	if s := fmt.Sprint(read); !strings.Contains(s, "fb_ping") {
		t.Fatalf("policy resource unreadable: %v", read)
	}

	// structured job status + wait mode round-trip
	out := c.callTool(t, "fb_demo_write", map[string]any{"db": "spike5"})
	rid := mustFind(t, out, `Request ID: ([0-9a-f]+)`)
	tok := mustFind(t, out, `token \(Tier 1 only\): ([0-9a-f]+)`)
	cout := c.callTool(t, "fb_confirm", map[string]any{"request_id": rid, "token": tok, "wait": true})
	if !strings.Contains(cout, "succeeded") {
		t.Fatalf("wait mode did not reach terminal success: %q", cout)
	}
	jobID := mustFind(t, cout, `([a-z0-9]+): succeeded`)
	st := c.roundTrip(t, "tools/call", map[string]any{"name": "fb_job_status", "arguments": map[string]any{"job_id": jobID}})
	if _, ok := st["structuredContent"].(map[string]any); !ok {
		t.Fatalf("fb_job_status has no structuredContent: %v", st)
	}
}
