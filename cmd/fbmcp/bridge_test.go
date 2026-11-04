package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/transport"
)

// Phase 12 B4 tests: the stdio→HTTPS bridge.

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitFor polls out until it contains every want, or fails.
func waitFor(t *testing.T, out *syncBuf, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s := out.String()
		ok := true
		for _, w := range want {
			if !strings.Contains(s, w) {
				ok = false
				break
			}
		}
		if ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("output lacks %q; got:\n%s", want, out.String())
}

func pinOf(srv *httptest.Server) string {
	h := sha256.Sum256(srv.Certificate().RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(h[:])
}

// fakeMCP is a minimal streamable-HTTP server: it can forget sessions
// (server restart) and records what the bridge sends.
type fakeMCP struct {
	mu       sync.Mutex
	sessions map[string]bool
	n        int
	deleted  []string
	protoHdr []string
	auth     []string
}

func (f *fakeMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	sid := r.Header.Get("Mcp-Session-Id")
	switch r.Method {
	case http.MethodDelete:
		f.deleted = append(f.deleted, sid)
		w.WriteHeader(200)
		return
	case http.MethodGet:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	json.Unmarshal(body, &m)
	if m.Method == "initialize" {
		f.n++
		sid = fmt.Sprintf("s%d", f.n)
		f.sessions[sid] = true
		w.Header().Set("Mcp-Session-Id", sid)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"fake","version":"0"}}}`, m.ID)
		return
	}
	if !f.sessions[sid] {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	f.protoHdr = append(f.protoHdr, r.Header.Get("Mcp-Protocol-Version"))
	if len(m.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	// Answer as SSE, the data split over two lines, like a pretty-printer.
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "event: message\nid: 1\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\ndata: \"result\":{\"session\":%q}}\n\n", m.ID, sid)
}

func startBridge(t *testing.T, url, key string, hc *http.Client) (*io.PipeWriter, *syncBuf, *syncBuf, chan struct{}) {
	t.Helper()
	inR, inW := io.Pipe()
	out, errw := &syncBuf{}, &syncBuf{}
	b := newBridge(url, key, hc, inR, out, errw)
	b.getBackoff = 10 * time.Millisecond
	done := make(chan struct{})
	go func() { b.run(context.Background()); close(done) }()
	return inW, out, errw, done
}

func send(t *testing.T, w io.Writer, line string) {
	t.Helper()
	if _, err := io.WriteString(w, line+"\n"); err != nil {
		t.Fatal(err)
	}
}

const initLine = `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`

func TestBridgeRelayReinitAndDelete(t *testing.T) {
	f := &fakeMCP{sessions: map[string]bool{}}
	srv := httptest.NewTLSServer(f)
	defer srv.Close()
	tlsCfg, err := bridgeTLS(pinOf(srv), "")
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	in, out, _, done := startBridge(t, srv.URL+"/mcp", "k1", hc)

	send(t, in, initLine)
	send(t, in, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(t, in, `{"jsonrpc":"2.0","id":7,"method":"tools/list"}`)
	waitFor(t, out, `"id":0`, `{"jsonrpc":"2.0","id":7,"result":{"session":"s1"}}`)

	// Server restart: the session is gone. One re-initialize, then the call
	// is sent again; the replayed initialize answer is not forwarded.
	f.mu.Lock()
	f.sessions = map[string]bool{}
	f.mu.Unlock()
	send(t, in, `{"jsonrpc":"2.0","id":8,"method":"tools/list"}`)
	waitFor(t, out, `{"jsonrpc":"2.0","id":8,"result":{"session":"s2"}}`)
	if strings.Count(out.String(), `"protocolVersion"`) != 1 {
		t.Fatalf("re-initialize answer leaked to the client:\n%s", out.String())
	}
	in.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not stop on stdin EOF")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.deleted) != 1 || f.deleted[0] != "s2" {
		t.Fatalf("DELETE on EOF: %v, want [s2]", f.deleted)
	}
	for _, a := range f.auth {
		if a != "Bearer k1" {
			t.Fatalf("Authorization header %q", a)
		}
	}
	for _, p := range f.protoHdr {
		if p != "2025-06-18" {
			t.Fatalf("Mcp-Protocol-Version %q", p)
		}
	}
}

func TestBridgePinMismatch(t *testing.T) {
	srv := httptest.NewTLSServer(&fakeMCP{sessions: map[string]bool{}})
	defer srv.Close()
	tlsCfg, _ := bridgeTLS("sha256:"+strings.Repeat("00", 32), "")
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	in, out, _, done := startBridge(t, srv.URL+"/mcp", "k1", hc)
	send(t, in, initLine)
	waitFor(t, out, `"id":0`, "pin mismatch")
	in.Close()
	<-done
	for _, bad := range []string{"sha256:xyz", "md5:" + strings.Repeat("00", 32), "sha256:00"} {
		if _, err := parsePin(bad); err == nil {
			t.Errorf("pin %q accepted", bad)
		}
	}
}

func TestBridgeHTTPErrorsAreClear(t *testing.T) {
	cases := []struct {
		code int
		hdr  string
		want string
	}{
		{401, "", "API key"},
		{403, "", "allow_from"},
		{429, "60", "retry after 60"},
	}
	for _, c := range cases {
		rr := httptest.NewRecorder()
		if c.hdr != "" {
			rr.Header().Set("Retry-After", c.hdr)
		}
		rr.WriteHeader(c.code)
		if err := httpError(rr.Result(), "x"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%d: %v, want %q", c.code, err, c.want)
		}
	}
}

// End to end with the real go-sdk handler behind the real authenticator:
// the bridge is wire-compatible with fbmcp's /mcp.
func TestBridgeAgainstRealServer(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "p12", Version: "0"}, nil)
	type echoIn struct {
		Text string `json:"text"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "echo"}, func(ctx context.Context, req *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo:" + in.Text}}}, nil, nil
	})
	auth := transport.NewAuthenticator([]config.APIIdentity{{Name: "op", KeyEnv: "X", MaxTier: config.Tier(1)}},
		map[string]string{"op": "k1"}, []string{"https://x"}, config.Limits{})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	srv := httptest.NewTLSServer(auth.Handler(h))
	defer srv.Close()
	tlsCfg, _ := bridgeTLS(pinOf(srv), "")
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	in, out, errw, done := startBridge(t, srv.URL+"/mcp", "k1", hc)

	send(t, in, initLine)
	send(t, in, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(t, in, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`)
	waitFor(t, out, `"id":0`, `"id":2`, `echo:hi`)
	if live := auth.LiveSessions()["op"]; live != 1 {
		t.Fatalf("live sessions on the server: %d, want 1", live)
	}
	in.Close()
	<-done
	if live := auth.LiveSessions()["op"]; live != 0 {
		t.Fatalf("session not closed by DELETE on EOF: %d live; stderr:\n%s", live, errw.String())
	}
}

func TestBridgeRejectsPlainHTTP(t *testing.T) {
	if code := runBridge([]string{"--url", "http://srv:8443/mcp", "--key-env", "K"}); code != 2 {
		t.Fatalf("http:// url: exit %d, want 2", code)
	}
}

// L1 finding: the go-sdk client probes with server/discover, whose _meta
// names the protocol version; the server then requires the same value in
// Mcp-Protocol-Version. The bridge must copy it, so a real client connects
// with no 400 in between.
func TestBridgeGoSDKClientNoErrors(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "p12", Version: "0"}, nil)
	auth := transport.NewAuthenticator([]config.APIIdentity{{Name: "op", KeyEnv: "X", MaxTier: config.Tier(1)}},
		map[string]string{"op": "k1"}, []string{"https://x"}, config.Limits{})
	srv := httptest.NewTLSServer(auth.Handler(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)))
	defer srv.Close()
	tlsCfg, _ := bridgeTLS(pinOf(srv), "")
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	errw := &syncBuf{}
	b := newBridge(srv.URL+"/mcp", "k1", hc, inR, outW, errw)
	done := make(chan struct{})
	go func() { b.run(context.Background()); outW.Close(); close(done) }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		t.Fatalf("connect through the bridge: %v; stderr:\n%s", err, errw.String())
	}
	if _, err := cs.ListTools(ctx, nil); err != nil {
		t.Fatal(err)
	}
	cs.Close()
	<-done
	if s := errw.String(); strings.Contains(s, "HTTP 4") {
		t.Fatalf("bridge reported an HTTP error:\n%s", s)
	}
}
