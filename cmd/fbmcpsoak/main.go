// fbmcpsoak — soak-week bootstrap (phase8_plan D1.3): a one-shot MCP client
// over the attach socket that creates the nightly_verify schedule grant for
// every registered database (Tier 1, in-band confirm), then exits. The
// unattended week itself is driven by packaging/soak-week.ps1 (phase 8M/M2).
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aleks/fbmcp/internal/attach"
	"github.com/aleks/fbmcp/internal/config"
)

func main() {
	cfgPath := flag.String("config", "fbmcp.yaml", "fbmcp configuration")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fbmcpsoak: %v\n", err)
		os.Exit(1)
	}
	conn, err := attach.Dial(cfg.State.Dir, 60*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fbmcpsoak: attach (is the kernel running?): %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()
	c := &client{conn: conn, dec: bufio.NewReader(conn), next: 1}
	if _, err := c.roundTrip("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "fbmcpsoak", "version": "0"},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "fbmcpsoak: initialize: %v\n", err)
		os.Exit(1)
	}
	c.notify("notifications/initialized")

	rc := 0
	for _, db := range cfg.Databases {
		out, err := c.callTool("fb_schedule_create", map[string]any{
			"db": db.ID, "target": "nightly_verify", "cron": "15 3 * * *", "timezone": "UTC",
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "fbmcpsoak: %s: %v\n", db.ID, err)
			rc = 1
			continue
		}
		rid, tok := extract(out)
		if rid == "" || tok == "" {
			fmt.Fprintf(os.Stderr, "fbmcpsoak: %s: no request id/token in %q\n", db.ID, out)
			rc = 1
			continue
		}
		cout, err := c.callTool("fb_confirm", map[string]any{"request_id": rid, "token": tok})
		if err != nil || !contains(cout, "confirmed") {
			fmt.Fprintf(os.Stderr, "fbmcpsoak: %s: confirm: %v %q\n", db.ID, err, cout)
			rc = 1
			continue
		}
		fmt.Printf("soak grant ready: %s nightly_verify 03:15 UTC\n", db.ID)
	}
	os.Exit(rc)
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

var (
	reRID = regexp.MustCompile(`Request ID: ([0-9a-f]+)`)
	reTok = regexp.MustCompile(`token \(Tier 1 only\): ([0-9a-f]+)`)
)

func extract(out string) (rid, tok string) {
	if m := reRID.FindStringSubmatch(out); len(m) == 2 {
		rid = m[1]
	}
	if m := reTok.FindStringSubmatch(out); len(m) == 2 {
		tok = m[1]
	}
	return rid, tok
}

type client struct {
	conn net.Conn
	dec  *bufio.Reader
	next int
}

func (c *client) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = c.conn.Write(append(b, '\n'))
	return err
}

func (c *client) notify(method string) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	c.conn.Write(append(b, '\n'))
}

func (c *client) roundTrip(method string, params any) (map[string]any, error) {
	id := c.next
	c.next++
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		line, err := c.dec.ReadString('\n')
		if err != nil {
			return nil, err
		}
		var msg map[string]any
		if json.Unmarshal([]byte(line), &msg) != nil {
			continue
		}
		if fmt.Sprint(msg["id"]) != fmt.Sprint(id) {
			continue
		}
		if e, ok := msg["error"].(map[string]any); ok {
			return nil, fmt.Errorf("%s: %v", method, e)
		}
		res, _ := msg["result"].(map[string]any)
		if res == nil {
			return nil, fmt.Errorf("%s: missing result: %s", method, line)
		}
		return res, nil
	}
}

func (c *client) callTool(name string, args map[string]any) (string, error) {
	res, err := c.roundTrip("tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", err
	}
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
