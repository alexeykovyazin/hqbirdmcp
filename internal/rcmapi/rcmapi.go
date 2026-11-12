// Package rcmapi is the client of the HQbird cluster manager (hqbirdrcm)
// operator API: HTTPS with a certificate of the cluster CA and HTTP Digest
// login (RCM refuses Basic). The paths and bodies follow the RCM CLI
// (hqcluster3 cmd/hqbirdrcm), which drives the same API.
package rcmapi

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- HTTP Digest (RFC 7616 MD5) is what RCM speaks
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Realm is the Digest realm of RCM; the challenge normally names it.
const Realm = "hqbirdrcm"

// maxBody bounds an RCM answer read into memory.
const maxBody = 8 << 20

// Client calls one RCM.
type Client struct {
	base  *url.URL
	user  string
	pass  string
	actor string
	hc    *http.Client
}

// New builds a client. caFile is the cluster CA (PEM) that signed the RCM
// certificate; "" uses the system roots.
func New(baseURL, caFile, user, pass string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil {
		return nil, fmt.Errorf("rcm url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("rcm url: scheme must be http(s)")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile) // #nosec G304 -- path from fbmcp.yaml
		if err != nil {
			return nil, fmt.Errorf("rcm ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("rcm ca_file %s: no PEM certificate", caFile)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &Client{base: u, user: user, pass: pass, hc: &http.Client{Transport: tr, Timeout: 120 * time.Second}}, nil
}

// WithActor returns a copy that names the fbmcp caller in X-Admin-User:
// the RCM audit then says on whose behalf fbmcp acted.
func (c *Client) WithActor(name string) *Client {
	cp := *c
	cp.actor = name
	return &cp
}

// SetHTTPClient replaces the HTTP client (tests).
func (c *Client) SetHTTPClient(hc *http.Client) { c.hc = hc }

// BaseURL is the RCM address of this client.
func (c *Client) BaseURL() string { return c.base.String() }

// Error is an answer of RCM other than 2xx.
type Error struct {
	Status  int
	Code    string
	Message string
	// Body is the whole answer: some refusals carry more than the message
	// (preflight details, the command for an administrator).
	Body json.RawMessage
}

func (e *Error) Error() string {
	switch {
	case e.Status == http.StatusUnauthorized:
		return "rcm login failed (401): the user or the password in the fbmcp secret is wrong. If the password was changed in the RCM UI, store the new one: fbmcp ctl secret set FBMCP_RCM_PASSWORD (the value comes from stdin)"
	case e.Status == http.StatusTooManyRequests:
		return "rcm throttled the login (429): too many failed logins; wait 5 minutes and check the password in the fbmcp secret"
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("rcm %d %s: %s", e.Status, e.Code, e.Message)
	case e.Message != "":
		return fmt.Sprintf("rcm %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("rcm %d: %s", e.Status, strings.TrimSpace(string(e.Body)))
}

// IsCode reports whether err is an RCM error with this code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Do sends one request and returns the body of a 2xx answer. body is sent
// as JSON when it is not nil.
func (c *Client) Do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = b
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("rcm path %q: must start with /", path)
	}
	target, err := url.Parse(c.base.String() + path)
	if err != nil {
		return nil, err
	}
	resp, err := c.send(ctx, method, target, payload, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		chal := resp.Header.Get("WWW-Authenticate")
		drain(resp)
		if !strings.HasPrefix(strings.ToLower(chal), "digest ") {
			return nil, &Error{Status: resp.StatusCode, Message: "RCM asked for a login that is not Digest"}
		}
		resp, err = c.send(ctx, method, target, payload, c.digest(method, target, chal))
		if err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, decodeError(resp.StatusCode, raw)
	}
	return raw, nil
}

func (c *Client) send(ctx context.Context, method string, target *url.URL, payload []byte, authz string) (*http.Response, error) {
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.actor != "" {
		req.Header.Set("X-Admin-User", c.actor)
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rcm %s: %w", c.base.Host, err)
	}
	return resp, nil
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// digest answers a Digest challenge (RFC 7616, MD5, qop=auth).
func (c *Client) digest(method string, target *url.URL, chal string) string {
	p := parseChallenge(chal)
	realm := p["realm"]
	if realm == "" {
		realm = Realm
	}
	nonce := p["nonce"]
	uri := target.RequestURI()
	ha1 := md5Hex(c.user + ":" + realm + ":" + c.pass)
	ha2 := md5Hex(method + ":" + uri)
	h := fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", algorithm=MD5`, c.user, realm, nonce, uri)
	if strings.Contains(p["qop"], "auth") {
		cnonce := randomHex(8)
		const nc = "00000001"
		h += fmt.Sprintf(`, response="%s", qop=auth, nc=%s, cnonce="%s"`, md5Hex(ha1+":"+nonce+":"+nc+":"+cnonce+":auth:"+ha2), nc, cnonce)
	} else {
		h += fmt.Sprintf(`, response="%s"`, md5Hex(ha1+":"+nonce+":"+ha2))
	}
	if op := p["opaque"]; op != "" {
		h += fmt.Sprintf(`, opaque="%s"`, op)
	}
	return h
}

// parseChallenge splits `Digest k="v", k2=v2` into a map.
func parseChallenge(h string) map[string]string {
	out := map[string]string{}
	h = strings.TrimSpace(h)
	if i := strings.IndexByte(h, ' '); i >= 0 {
		h = h[i+1:]
	}
	for len(h) > 0 {
		h = strings.TrimLeft(h, " ,")
		eq := strings.IndexByte(h, '=')
		if eq < 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(h[:eq]))
		h = h[eq+1:]
		var val string
		if strings.HasPrefix(h, `"`) {
			end := strings.IndexByte(h[1:], '"')
			if end < 0 {
				val, h = h[1:], ""
			} else {
				val, h = h[1:end+1], h[end+2:]
			}
		} else {
			end := strings.IndexByte(h, ',')
			if end < 0 {
				val, h = h, ""
			} else {
				val, h = h[:end], h[end:]
			}
		}
		out[key] = strings.TrimSpace(val)
	}
	return out
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s)) // #nosec G401 -- HTTP Digest
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func decodeError(status int, raw []byte) error {
	e := &Error{Status: status, Body: append(json.RawMessage(nil), raw...)}
	var doc struct {
		Error json.RawMessage `json:"error"`
		Code  string          `json:"code"`
		Msg   string          `json:"message"`
	}
	if json.Unmarshal(raw, &doc) == nil {
		var inner struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		switch {
		case len(doc.Error) > 0 && json.Unmarshal(doc.Error, &inner) == nil:
			e.Code, e.Message = inner.Code, inner.Message
		case len(doc.Error) > 0:
			_ = json.Unmarshal(doc.Error, &e.Message)
			e.Code = doc.Code
		default:
			e.Code, e.Message = doc.Code, doc.Msg
		}
	}
	if e.Message == "" && e.Code == "" {
		e.Message = strings.TrimSpace(string(raw))
		if len(e.Message) > 300 {
			e.Message = e.Message[:300] + "…"
		}
	}
	return e
}

// PathEscape escapes one path segment (node ids, db ids, job ids).
func PathEscape(s string) string { return url.PathEscape(s) }
