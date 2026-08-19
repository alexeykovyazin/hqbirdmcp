package attach

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/instlock"
)

func TestTwoClientsOneKernel(t *testing.T) {
	dir := t.TempDir()
	lock, err := instlock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	server := mcp.NewServer(&mcp.Implementation{Name: "fbmcp", Version: "test"}, nil)
	type noArgs struct{}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_ping", Description: "liveness"}, func(ctx context.Context, req *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
	})

	stop, err := Start(dir, func(c net.Conn) {
		_ = server.Run(context.Background(), &mcp.IOTransport{Reader: c, Writer: c})
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	second, err := instlock.Acquire(dir)
	if err == nil {
		second.Release()
		t.Fatal("C4 FAIL: second kernel acquired the state lock")
	}

	ping := func(label string) {
		t.Helper()
		conn, err := Dial(dir, 3*time.Second)
		if err != nil {
			t.Fatalf("%s dial: %v", label, err)
		}
		defer conn.Close()
		client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
		sess, err := client.Connect(context.Background(), &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
		if err != nil {
			t.Fatalf("%s connect: %v", label, err)
		}
		defer sess.Close()
		res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "fb_ping"})
		if err != nil {
			t.Fatalf("%s ping: %v", label, err)
		}
		if len(res.Content) == 0 {
			t.Fatalf("%s empty ping", label)
		}
		tc, _ := res.Content[0].(*mcp.TextContent)
		if tc == nil || tc.Text != "pong" {
			t.Fatalf("%s got %#v", label, res.Content[0])
		}
	}
	ping("a")
	ping("b")
}

func TestBadTokenRejected(t *testing.T) {
	dir := t.TempDir()
	stop, err := Start(dir, func(c net.Conn) {
		t.Error("session must not start with a bad token")
		c.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	addr, err := os.ReadFile(filepath.Join(dir, addrFile))
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTimeout("tcp", strings.TrimSpace(string(addr)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("FBMCP1 deadbeef\n"))
	buf := make([]byte, 8)
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, rerr := c.Read(buf)
	if rerr == nil {
		t.Fatal("expected the primary to drop a bad-token conn")
	}
}

func TestSpliceEcho(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	stdin := bytes.NewBufferString("hello")
	var stdout bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- Splice(a, stdin, &stdout) }()
	buf := make([]byte, 5)
	if _, err := io.ReadFull(b, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "hello" {
		t.Fatalf("got %q", buf)
	}
	if _, err := b.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	b.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("splice did not return")
	}
	if stdout.String() != "ok" {
		t.Fatalf("stdout %q", stdout.String())
	}
}

func TestDialWaitsForPrimary(t *testing.T) {
	dir := t.TempDir()
	errc := make(chan error, 1)
	go func() {
		c, err := Dial(dir, 3*time.Second)
		if c != nil {
			c.Close()
		}
		errc <- err
	}()
	time.Sleep(100 * time.Millisecond)
	stop, err := Start(dir, func(c net.Conn) { io.Copy(io.Discard, c) })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("dial did not succeed after Start")
	}
}
