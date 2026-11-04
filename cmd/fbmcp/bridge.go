package main

// fbmcp bridge (phase 12 B4): a stdio MCP server for the client (Claude
// Desktop, Cursor, …) that relays every message to a remote fbmcp over
// HTTPS (streamable HTTP, /mcp). It is a plain HTTP relay, not the go-sdk
// client transport (F8): it does not parse MCP methods beyond initialize,
// so tools, resources, prompts, progress and elicitation pass unchanged.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aleks/fbmcp/internal/secrets"
)

const (
	hdrSession  = "Mcp-Session-Id"
	hdrProtocol = "Mcp-Protocol-Version"
	// protocolNoGET: from this version on the server has no standalone GET
	// stream (SEP-2575).
	protocolNoGET = "2026-07-28"
	// metaProtocol: a request that names its protocol version in _meta
	// (SEP-2575, e.g. the go-sdk client's server/discover probe) must send
	// the same value in the Mcp-Protocol-Version header.
	metaProtocol = "io.modelcontextprotocol/protocolVersion"
)

func runBridge(args []string) int {
	fs := flag.NewFlagSet("bridge", flag.ContinueOnError)
	url := fs.String("url", "", "remote endpoint, e.g. https://srv1:8443/mcp")
	keyEnv := fs.String("key-env", "", "secret name of the API key (env, then --secrets-dir file, then OS keyring)")
	pin := fs.String("pin", "", "sha256:<hex> of the server public key (fbmcpctl tls selfsign / tls show); replaces CA checks")
	caFile := fs.String("ca", "", "PEM file with the CA (or the self-signed cert) to trust, instead of the system roots")
	secDir := fs.String("secrets-dir", "", "folder with one file per secret name (optional)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *url == "" || *keyEnv == "" {
		fmt.Fprintln(os.Stderr, "usage: fbmcp bridge --url https://srv1:8443/mcp --key-env FBMCP_KEY_SRV1 [--pin sha256:<hex> | --ca srv1.crt] [--secrets-dir DIR]")
		return 2
	}
	if !strings.HasPrefix(strings.ToLower(*url), "https://") {
		fmt.Fprintln(os.Stderr, "fbmcp bridge: --url must be https:// (the key must not travel in clear text)")
		return 2
	}
	secrets.SetDir(*secDir)
	key, err := secrets.Get(*keyEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fbmcp bridge: API key: %v\n", err)
		return 1
	}
	tlsCfg, err := bridgeTLS(*pin, *caFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fbmcp bridge: %v\n", err)
		return 2
	}
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg, ForceAttemptHTTP2: true,
		TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 0, IdleConnTimeout: 90 * time.Second}}
	b := newBridge(*url, key, hc, os.Stdin, os.Stdout, os.Stderr)
	if err := b.run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "fbmcp bridge: %v\n", err)
		return 1
	}
	return 0
}

// bridgeTLS builds the client TLS config: a public-key pin (checked on
// every handshake; chain and names are not checked), a CA file, or the
// system roots.
func bridgeTLS(pin, caFile string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if pin != "" {
		want, err := parsePin(pin)
		if err != nil {
			return nil, err
		}
		cfg.InsecureSkipVerify = true // replaced by the pin check below
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("pin check: server sent no certificate")
			}
			got := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if subtle.ConstantTimeCompare(got[:], want) != 1 {
				return fmt.Errorf("pin mismatch: server key is sha256:%s, expected %s — the server key changed or this is not your server", hex.EncodeToString(got[:]), pin)
			}
			return nil
		}
		return cfg, nil
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("--ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("--ca %s: no PEM certificate found", caFile)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

func parsePin(pin string) ([]byte, error) {
	h, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(pin)), "sha256:")
	if !ok {
		return nil, fmt.Errorf("--pin must look like sha256:<64 hex chars>")
	}
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != sha256.Size {
		return nil, fmt.Errorf("--pin must look like sha256:<64 hex chars>")
	}
	return b, nil
}

type bridge struct {
	url  string
	key  string
	hc   *http.Client
	in   io.Reader
	out  io.Writer
	errw io.Writer

	outMu sync.Mutex

	mu         sync.Mutex
	session    string
	protocol   string
	initLine   []byte // the client's initialize request, for a re-initialize
	initGen    int    // bumps on every (re-)initialize
	getRunning bool
	closed     bool

	reinitMu sync.Mutex
	wg       sync.WaitGroup
	// maxGetRetries bounds GET stream reconnects (B4: at most 5).
	maxGetRetries int
	getBackoff    time.Duration
}

func newBridge(url, key string, hc *http.Client, in io.Reader, out, errw io.Writer) *bridge {
	return &bridge{url: url, key: key, hc: hc, in: in, out: out, errw: errw, maxGetRetries: 5, getBackoff: time.Second}
}

func (b *bridge) logf(format string, args ...any) {
	fmt.Fprintf(b.errw, "fbmcp bridge: "+format+"\n", args...)
}

// emit writes one JSON-RPC message as one stdout line.
func (b *bridge) emit(msg []byte) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, msg); err != nil {
		buf.Reset()
		buf.Write(bytes.TrimSpace(msg))
	}
	buf.WriteByte('\n')
	b.outMu.Lock()
	_, _ = b.out.Write(buf.Bytes())
	b.outMu.Unlock()
}

type rpcHead struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params *struct {
		Meta map[string]any `json:"_meta"`
		Name string         `json:"name"` // tools/call, prompts/get
		URI  string         `json:"uri"`  // resources/read
	} `json:"params,omitempty"`
	Result *struct {
		ProtocolVersion string `json:"protocolVersion"`
	} `json:"result,omitempty"`
}

// metaVersion returns the request's _meta protocol version, if any.
func (h rpcHead) metaVersion() string {
	if h.Params == nil {
		return ""
	}
	v, _ := h.Params.Meta[metaProtocol].(string)
	return v
}

func (b *bridge) emitError(id json.RawMessage, code int, msg string) {
	if len(id) == 0 || string(id) == "null" {
		return // a notification gets no answer
	}
	out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg}})
	b.emit(out)
}

func (b *bridge) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rd := bufio.NewReaderSize(b.in, 1<<20)
	for {
		line, err := rd.ReadBytes('\n')
		if t := bytes.TrimSpace(line); len(t) > 0 {
			msg := append([]byte(nil), t...)
			var h rpcHead
			_ = json.Unmarshal(msg, &h)
			if h.Method == "initialize" {
				// Serialize: later messages need the session id.
				b.handle(ctx, msg, h)
			} else {
				b.wg.Add(1)
				go func() {
					defer b.wg.Done()
					b.handle(ctx, msg, h)
				}()
			}
		}
		if err != nil {
			break // EOF: the client went away
		}
	}
	// Give in-flight calls a moment, then abort them: the client is gone.
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		cancel()
		<-done
	}
	b.mu.Lock()
	b.closed = true
	sid := b.session
	b.mu.Unlock()
	if sid != "" {
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dcancel()
		req, _ := http.NewRequestWithContext(dctx, http.MethodDelete, b.url, nil)
		b.headers(req, sid, b.protocolNow())
		if resp, err := b.hc.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	return nil
}

func (b *bridge) protocolNow() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.protocol
}

func (b *bridge) headers(req *http.Request, sid, proto string) {
	req.Header.Set("Authorization", "Bearer "+b.key)
	if sid != "" {
		req.Header.Set(hdrSession, sid)
	}
	if proto != "" {
		req.Header.Set(hdrProtocol, proto)
	}
}

// handle relays one client message and its answer(s).
func (b *bridge) handle(ctx context.Context, msg []byte, h rpcHead) {
	if h.Method == "initialize" {
		b.mu.Lock()
		b.initLine = msg
		b.session, b.protocol = "", ""
		b.mu.Unlock()
	}
	b.mu.Lock()
	gen := b.initGen
	b.mu.Unlock()
	status, err := b.post(ctx, msg, h, true)
	if status == http.StatusNotFound && h.Method != "initialize" {
		// Session gone (server restart or idle timeout, B0.5): one
		// re-initialize, then send the request again.
		if rerr := b.reinitialize(ctx, gen); rerr != nil {
			b.logf("re-initialize failed: %v", rerr)
			b.emitError(h.ID, -32001, "fbmcp bridge: session lost and re-initialize failed: "+rerr.Error())
			return
		}
		status, err = b.post(ctx, msg, h, true)
		if err == nil && status == http.StatusNotFound {
			err = errors.New("session not found again after re-initialize")
		}
	}
	if err != nil {
		b.logf("%v", err)
		b.emitError(h.ID, -32002, "fbmcp bridge: "+err.Error())
	}
}

// post sends one message. forward=false swallows the answer (internal
// re-initialize). It returns the HTTP status (0 = transport error).
func (b *bridge) post(ctx context.Context, msg []byte, h rpcHead, forward bool) (int, error) {
	b.mu.Lock()
	sid, proto := b.session, b.protocol
	b.mu.Unlock()
	if v := h.metaVersion(); v != "" {
		proto = v // the server checks header == _meta
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.url, bytes.NewReader(msg))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	b.headers(req, sid, proto)
	standardHeaders(req.Header, h, proto)
	resp, err := b.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("server unreachable: %v", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound && sid != "":
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	case resp.StatusCode >= 300:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return resp.StatusCode, httpError(resp, strings.TrimSpace(string(body)))
	}
	if h.Method == "initialize" {
		if s := resp.Header.Get(hdrSession); s != "" {
			b.mu.Lock()
			b.session = s
			b.mu.Unlock()
		}
	}
	if resp.StatusCode == http.StatusAccepted {
		return resp.StatusCode, nil
	}
	onMsg := func(m []byte) {
		var rh rpcHead
		if json.Unmarshal(m, &rh) == nil && h.Method == "initialize" && rh.Result != nil && string(rh.ID) == string(h.ID) {
			b.mu.Lock()
			b.protocol = rh.Result.ProtocolVersion
			b.initGen++
			b.mu.Unlock()
		}
		if forward {
			b.emit(m)
		}
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "text/event-stream") {
		readSSE(resp.Body, func(_ string, data []byte) { onMsg(data) })
	} else {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return resp.StatusCode, fmt.Errorf("read answer: %v", err)
		}
		body = bytes.TrimSpace(body)
		if len(body) > 0 && body[0] == '[' {
			var arr []json.RawMessage
			if json.Unmarshal(body, &arr) == nil {
				for _, m := range arr {
					onMsg(m)
				}
			}
		} else if len(body) > 0 {
			onMsg(body)
		}
	}
	if h.Method == "notifications/initialized" {
		b.startGET(ctx)
	}
	return resp.StatusCode, nil
}

// standardHeaders adds Mcp-Method and Mcp-Name, which the server requires
// from protocol 2026-07-28 on (SEP-2243). Mcp-Param-* headers come from
// x-mcp-header schema annotations; fbmcp tools have none.
func standardHeaders(hdr http.Header, h rpcHead, proto string) {
	if proto == "" || proto < protocolNoGET || h.Method == "" {
		return
	}
	hdr.Set("Mcp-Method", h.Method)
	if h.Params == nil {
		return
	}
	name := ""
	switch h.Method {
	case "tools/call", "prompts/get":
		name = h.Params.Name
	case "resources/read":
		name = h.Params.URI
	}
	if name != "" {
		hdr.Set("Mcp-Name", headerValue(name))
	}
}

// headerValue wraps a value that is not plain printable ASCII (or looks
// like the wrapper itself) as =?base64?...?=, per SEP-2243.
func headerValue(s string) string {
	plain := s[0] != ' ' && s[0] != '\t' && s[len(s)-1] != ' ' && s[len(s)-1] != '\t' &&
		!(strings.HasPrefix(s, "=?base64?") && strings.HasSuffix(s, "?="))
	for _, c := range s {
		if c < 0x20 || c > 0x7e {
			plain = false
			break
		}
	}
	if plain {
		return s
	}
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
}

// httpError maps an HTTP failure to a clear message (B4).
func httpError(resp *http.Response, body string) error {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return errors.New("401 unauthorized: the server does not accept this API key (wrong key, or not loaded on the server)")
	case http.StatusForbidden:
		return fmt.Errorf("403 forbidden: %s (source address outside allow_from, or session of another key)", body)
	case http.StatusTooManyRequests:
		ra := resp.Header.Get("Retry-After")
		return fmt.Errorf("429 too many requests: %s; retry after %s s", body, ra)
	default:
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
}

// reinitialize replays the cached initialize once per lost session.
func (b *bridge) reinitialize(ctx context.Context, seenGen int) error {
	b.reinitMu.Lock()
	defer b.reinitMu.Unlock()
	b.mu.Lock()
	if b.initGen != seenGen { // another request already re-initialized
		b.mu.Unlock()
		return nil
	}
	initLine := b.initLine
	b.session, b.protocol = "", ""
	b.mu.Unlock()
	if initLine == nil {
		return errors.New("no initialize request to replay")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(initLine, &m); err != nil {
		return err
	}
	m["id"] = json.RawMessage(fmt.Sprintf(`"fbmcp-bridge-reinit-%d"`, seenGen+1))
	replay, _ := json.Marshal(m)
	var h rpcHead
	_ = json.Unmarshal(replay, &h)
	if st, err := b.post(ctx, replay, h, false); err != nil || st >= 300 {
		if err == nil {
			err = fmt.Errorf("HTTP %d", st)
		}
		return err
	}
	b.logf("session re-initialized")
	note := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	_, err := b.post(ctx, note, rpcHead{Method: "notifications/initialized"}, false)
	return err
}

// startGET opens the standalone server→client stream once per session,
// unless the protocol has none.
func (b *bridge) startGET(ctx context.Context) {
	b.mu.Lock()
	if b.getRunning || b.closed || b.session == "" || (b.protocol != "" && b.protocol >= protocolNoGET) {
		b.mu.Unlock()
		return
	}
	b.getRunning = true
	b.mu.Unlock()
	go b.getLoop(ctx)
}

func (b *bridge) getLoop(ctx context.Context) {
	defer func() {
		b.mu.Lock()
		b.getRunning = false
		b.mu.Unlock()
	}()
	retries := 0
	lastID := ""
	for {
		b.mu.Lock()
		sid, proto, closed := b.session, b.protocol, b.closed
		b.mu.Unlock()
		if closed || sid == "" || ctx.Err() != nil {
			return
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.url, nil)
		req.Header.Set("Accept", "text/event-stream")
		b.headers(req, sid, proto)
		if lastID != "" {
			req.Header.Set("Last-Event-ID", lastID)
		}
		started := time.Now()
		resp, err := b.hc.Do(req)
		if err == nil {
			switch {
			case resp.StatusCode == http.StatusOK:
				readSSE(resp.Body, func(id string, data []byte) {
					if id != "" {
						lastID = id
					}
					b.emit(data)
				})
				resp.Body.Close()
			case resp.StatusCode == http.StatusMethodNotAllowed:
				resp.Body.Close()
				return // the server has no GET stream
			case resp.StatusCode == http.StatusNotFound:
				resp.Body.Close()
				return // session gone; the next POST re-initializes and restarts the stream
			default:
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
				resp.Body.Close()
				b.logf("GET stream: %v", httpError(resp, strings.TrimSpace(string(body))))
			}
		}
		if time.Since(started) > time.Minute {
			retries = 0 // a stream that lived a while is not a failure loop
		}
		retries++
		if retries > b.maxGetRetries {
			b.logf("GET stream: gave up after %d reconnects (server notifications stop; calls still work)", b.maxGetRetries)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(b.getBackoff * time.Duration(retries)):
		}
	}
}

// readSSE calls fn for each event with its data (data lines joined by \n).
func readSSE(r io.Reader, fn func(id string, data []byte)) {
	rd := bufio.NewReaderSize(r, 1<<20)
	var data bytes.Buffer
	id := ""
	for {
		line, err := rd.ReadBytes('\n')
		l := bytes.TrimRight(line, "\r\n")
		switch {
		case len(l) == 0 && len(line) > 0:
			if data.Len() > 0 {
				fn(id, append([]byte(nil), data.Bytes()...))
			}
			data.Reset()
		case bytes.HasPrefix(l, []byte("data:")):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimPrefix(bytes.TrimPrefix(l, []byte("data:")), []byte(" ")))
		case bytes.HasPrefix(l, []byte("id:")):
			id = strings.TrimSpace(string(l[3:]))
		}
		if err != nil {
			if data.Len() > 0 {
				fn(id, append([]byte(nil), data.Bytes()...))
			}
			return
		}
	}
}
