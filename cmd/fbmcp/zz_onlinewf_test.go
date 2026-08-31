package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/attach"
	"github.com/aleks/fbmcp/internal/config"
)

type probeClient struct {
	conn net.Conn
	dec  *bufio.Reader
	next int
}

func (c *probeClient) send(v any) error {
	b, _ := json.Marshal(v)
	_, err := c.conn.Write(append(b, '\n'))
	return err
}
func (c *probeClient) notify(method string) { c.send(map[string]any{"jsonrpc": "2.0", "method": method}) }
func (c *probeClient) roundTrip(method string, params any) (string, error) {
	c.next++
	c.send(map[string]any{"jsonrpc": "2.0", "id": c.next, "method": method, "params": params})
	return c.dec.ReadString('\n')
}
func (c *probeClient) callTool(name string, args map[string]any) (string, error) {
	return c.roundTrip("tools/call", map[string]any{"name": name, "arguments": args})
}

func TestOnlineSpike5(t *testing.T) {
	if os.Getenv("FBMCP_ONLINE") == "" {
		t.Skip("set FBMCP_ONLINE=1")
	}
	cfg, err := config.Load("../../fbmcp.dev.yaml")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := attach.Dial(cfg.State.Dir, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := &probeClient{conn: conn, dec: bufio.NewReader(conn), next: 1}
	c.roundTrip("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "online", "version": "0"}})
	c.notify("notifications/initialized")

	out, err := c.callTool("fb_shutdown_window", map[string]any{"db": "spike5"})
	if err != nil {
		t.Fatal(err)
	}
	rid := regexp.MustCompile(`Request ID: ([0-9a-f]+)`).FindStringSubmatch(out)
	if rid == nil {
		t.Fatalf("still denied: %.300s", out)
	}
	marker := filepath.Join(cfg.State.Dir, "approvals", rid[1])
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("approval marker written: %s", marker)

	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		q, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 999,
			"method": "tools/call", "params": map[string]any{"name": "fb_transactions",
				"arguments": map[string]any{"db": "spike5"}}})
		pc2, err := attach.Dial(cfg.State.Dir, 5*time.Second)
		if err != nil {
			continue
		}
		pc2.Write(append(q, '\n'))
		pc2.SetReadDeadline(time.Now().Add(10 * time.Second))
		line, _ := bufio.NewReader(pc2).ReadString('\n')
		pc2.Close()
		if strings.Contains(line, "OIT") {
			t.Logf("database answering queries again — online")
			return
		}
	}
	t.Fatal("spike5 did not come back online within 3 min")
}
