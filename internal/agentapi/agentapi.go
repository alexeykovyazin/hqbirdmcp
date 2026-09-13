// Package agentapi is the HTTP client for the co-located fbagent Local API
// (phase9_plan.md, ADR-031; API reference: fbagent 2.21.0). Loopback +
// Bearer only. Every failure is surfaced as an error whose text starts with
// an "fbagent <code>" marker — the errdoc classifier (seeded with the same
// markers) turns those into structured {code, hint, remediation} envelopes.
// Responses decode into loose types/json.RawMessage on purpose: the agent
// evolves independently and unknown fields must never break the bridge.
package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxBody mirrors the agent's decoded response cap (8 MiB default) with a
// small slack so an over-cap body is detected as payload-too-large instead
// of silently truncated.
const maxBody = 8<<20 + 1024

// Client dials one agent's Local API. Construct per call site (cheap);
// never at tool-registration time (registration must stay hermetic).
type Client struct {
	base  string // scheme://host[:port], no trailing slash
	token string
	hc    *http.Client
}

// New builds a client for one agent endpoint.
func New(baseURL, token string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("agent base_url: %w", err)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return &Client{base: u.String(), token: token, hc: &http.Client{}}, nil
}

// do is the single request choke point: Bearer header, JSON in/out, and the
// error mapping every tool relies on.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("fbagent bad-request: encode body: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return fmt.Errorf("fbagent transport: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("fbagent transport: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("fbagent transport: read body: %w", err)
	}
	if resp.StatusCode < 300 && out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("fbagent service-error: %s %s: response is not valid JSON: %w", method, path, err)
		}
	}
	if resp.StatusCode >= 300 {
		return apiError(resp.StatusCode, data)
	}
	return nil
}

// apiError maps the agent's error envelope {"error":{"code","message"}} +
// HTTP status onto the errdoc markers. The agent's own code and message are
// preserved verbatim after the marker.
func apiError(status int, body []byte) error {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	msg := env.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200]
		}
	}
	code := env.Error.Code
	switch {
	case status == 401:
		return fmt.Errorf("fbagent auth (%d, %s): %s", status, code, msg)
	case code == "not_found" || status == 404:
		return fmt.Errorf("fbagent not-found (%d, %s): %s", status, code, msg)
	case code == "payload_too_large" || status == 413:
		return fmt.Errorf("fbagent payload-too-large (%d, %s): %s", status, code, msg)
	case code == "unavailable" || status == 503:
		return fmt.Errorf("fbagent unavailable (%d, %s): %s", status, code, msg)
	case code == "service_error" || status >= 500:
		return fmt.Errorf("fbagent service-error (%d, %s): %s", status, code, msg)
	default: // denied at 400/405 and anything unrecognized
		return fmt.Errorf("fbagent bad-request (%d, %s): %s", status, code, msg)
	}
}

// --- wire types (only what fbmcp acts on; everything display-only is
// json.RawMessage) ---

// Operation is the async operation document (API ref §5/§13).
type Operation struct {
	OperationID string `json:"operation_id"`
	Kind        string `json:"kind"`
	State       string `json:"state"` // queued|in_progress|succeeded|failed
	Error       *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Warnings           []string        `json:"warnings"`
	StatusURL          string          `json:"status_url"`
	Result             json.RawMessage `json:"result"`
	RestartOperationID string          `json:"restart_operation_id"`
}

// RestartOpID extracts the firebird-update nested restart operation id.
func (o Operation) RestartOpID() string {
	if o.RestartOperationID != "" {
		return o.RestartOperationID
	}
	var r struct {
		RestartOperationID string `json:"restart_operation_id"`
	}
	if json.Unmarshal(o.Result, &r) == nil {
		return r.RestartOperationID
	}
	return ""
}

// EngineOpRequest is the (optional) restart/stop/start body.
type EngineOpRequest struct {
	RequestedBy string `json:"requested_by,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Coalesce    *bool  `json:"coalesce,omitempty"`
}

// runResponse is the 202 shape shared by engine ops, job runs and update
// apply.
type runResponse struct {
	OperationID        string   `json:"operation_id"`
	Job                string   `json:"job"`
	StatusURL          string   `json:"status_url"`
	Warnings           []string `json:"warnings"`
	RestartOperationID string   `json:"restart_operation_id"`
}

// JobRunRequest is the unified POST /v1/jobs/{job}/run body.
type JobRunRequest struct {
	RequestedBy string         `json:"requested_by,omitempty"`
	Reason      string         `json:"reason,omitempty"`
	JobID       string         `json:"job_id,omitempty"`
	DBKeys      []string       `json:"db_keys,omitempty"`
	Params      map[string]any `json:"params,omitempty"`
}

// --- endpoint methods ---

// GetInstances lists (or, with id, details) the agent's managed instance(s).
func (c *Client) GetInstances(ctx context.Context, id string) (json.RawMessage, error) {
	if id == "" {
		var out json.RawMessage
		return out, c.do(ctx, http.MethodGet, "/v1/instances", nil, nil, &out)
	}
	var out json.RawMessage
	return out, c.do(ctx, http.MethodGet, "/v1/instances/"+url.PathEscape(id), nil, nil, &out)
}

// EngineOp posts restart/stop/start and returns the 202 operation handle.
func (c *Client) EngineOp(ctx context.Context, id, action string, body EngineOpRequest) (runResponse, error) {
	var out runResponse
	return out, c.do(ctx, http.MethodPost, "/v1/instances/"+url.PathEscape(id)+"/"+url.PathEscape(action), nil, body, &out)
}

// GetOperation polls one async operation (404 once pruned).
func (c *Client) GetOperation(ctx context.Context, opID string) (Operation, error) {
	var out Operation
	return out, c.do(ctx, http.MethodGet, "/v1/operations/"+url.PathEscape(opID), nil, nil, &out)
}

// GetStatus reads server_status.json (or a section / the health view).
func (c *Client) GetStatus(ctx context.Context, section, view string) (json.RawMessage, error) {
	q := url.Values{}
	if section != "" {
		q.Set("section", section)
	}
	if view != "" {
		q.Set("view", view)
	}
	var out json.RawMessage
	return out, c.do(ctx, http.MethodGet, "/v1/status", q, nil, &out)
}

// GetDatabases returns the latest active-databases artifact.
func (c *Client) GetDatabases(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.do(ctx, http.MethodGet, "/v1/databases", nil, nil, &out)
}

// ValidateScope mirrors the agent's pathguard: server scope, or a database
// folder key with no separators or dot-dot.
func ValidateScope(scope string) error {
	if scope == "server" {
		return nil
	}
	if scope == "" {
		return fmt.Errorf("scope is required (\"server\" or a database folder key)")
	}
	if strings.ContainsAny(scope, `/\`) || strings.Contains(scope, "..") {
		return fmt.Errorf("invalid scope %q: separators and .. are refused", scope)
	}
	return nil
}

// ValidateArtifactPath checks the slash-relative artifact path.
func ValidateArtifactPath(p string) error {
	if p == "" {
		return fmt.Errorf("artifact path is required")
	}
	if strings.Contains(p, "..") || strings.Contains(p, `\`) {
		return fmt.Errorf("invalid artifact path %q", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("invalid artifact path %q", p)
		}
	}
	return nil
}

// ArtifactQuery carries the endpoint-specific query params for GetArtifact.
type ArtifactQuery struct {
	Tail  int    // db-scoped */latest only
	Date  string // server lwm/summary only (YYYY-MM-DD)
	At    string // server lwm/summary|raw only (RFC3339)
	Limit int    // server config/snapshots (1-100) and traces/processed (<=200) only
}

// ValidateArtifactQuery enforces the per-endpoint query-param rules
// client-side so impossible combinations fail before the agent 400s.
func ValidateArtifactQuery(scope, path string, q ArtifactQuery) error {
	if q.Tail < 0 || q.Limit < 0 {
		return fmt.Errorf("tail/limit must be >= 0")
	}
	isServer := scope == "server"
	isLatest := strings.HasSuffix(path, "/latest")
	if q.Tail > 0 && (isServer || !isLatest) {
		return fmt.Errorf("tail applies only to database-scoped */latest artifacts")
	}
	if q.Date != "" && (!isServer || path != "lwm/summary") {
		return fmt.Errorf("date applies only to server lwm/summary")
	}
	if q.At != "" && (!isServer || (path != "lwm/summary" && path != "lwm/raw")) {
		return fmt.Errorf("at applies only to server lwm/summary and lwm/raw")
	}
	if q.Limit > 0 && (!isServer || (path != "config/snapshots" && path != "traces/processed")) {
		return fmt.Errorf("limit applies only to server config/snapshots and traces/processed")
	}
	return nil
}

// GetArtifact reads one artifact (listing routes need GetArtifactList).
func (c *Client) GetArtifact(ctx context.Context, scope, path string, q ArtifactQuery) (json.RawMessage, error) {
	v := url.Values{}
	if q.Tail > 0 {
		v.Set("tail", fmt.Sprint(q.Tail))
	}
	if q.Date != "" {
		v.Set("date", q.Date)
	}
	if q.At != "" {
		v.Set("at", q.At)
	}
	if q.Limit > 0 {
		v.Set("limit", fmt.Sprint(q.Limit))
	}
	var out json.RawMessage
	return out, c.do(ctx, http.MethodGet, "/v1/artifacts/"+scope+"/"+path, v, nil, &out)
}

// GetArtifactList runs a glob listing under server or a database folder.
// scope is a required path segment for BOTH kinds ("server" included) —
// probed 404 without it.
func (c *Client) GetArtifactList(ctx context.Context, scope, glob string) (json.RawMessage, error) {
	if scope == "" {
		return nil, fmt.Errorf("fbagent bad-request: scope is required")
	}
	v := url.Values{"glob": {glob}}
	var out json.RawMessage
	return out, c.do(ctx, http.MethodGet, "/v1/artifacts/"+scope, v, nil, &out)
}

// GetJobStatus reads a job family's persistent status document.
func (c *Client) GetJobStatus(ctx context.Context, job, db string) (json.RawMessage, error) {
	q := url.Values{}
	if db != "" {
		q.Set("db", db)
	}
	var out json.RawMessage
	return out, c.do(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(job)+"/status", q, nil, &out)
}

// RunJob triggers a job run; the 202 carries the operation to poll.
func (c *Client) RunJob(ctx context.Context, job string, req JobRunRequest) (runResponse, error) {
	var out runResponse
	return out, c.do(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(job)+"/run", nil, req, &out)
}

// ReloadConfig hot-reloads agent_config.json into the running scheduler.
func (c *Client) ReloadConfig(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.do(ctx, http.MethodPost, "/v1/config/reload", nil, map[string]any{}, &out)
}

// GetDBAccess reads the DatabaseAccess policy inventory.
func (c *Client) GetDBAccess(ctx context.Context, instID string) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.do(ctx, http.MethodGet, "/v1/instances/"+url.PathEscape(instID)+"/database-access", nil, nil, &out)
}

// CheckDBAccess evaluates one path against the policy (frozen CheckPath).
func (c *Client) CheckDBAccess(ctx context.Context, instID, path string) (json.RawMessage, error) {
	var out json.RawMessage
	body := map[string]string{"path": path}
	return out, c.do(ctx, http.MethodPost, "/v1/instances/"+url.PathEscape(instID)+"/database-access/check", nil, body, &out)
}

// GetUpdateStatus reads the firebird-updater runtime state sidecar.
func (c *Client) GetUpdateStatus(ctx context.Context, instID string) (json.RawMessage, error) {
	p := "/v1/instances/" + url.PathEscape(instID) + "/firebird-update"
	var out json.RawMessage
	return out, c.do(ctx, http.MethodGet, p, nil, nil, &out)
}

// ApplyUpdate applies a prepared Firebird update (async; may enqueue a
// nested restart — see Operation.RestartOpID).
func (c *Client) ApplyUpdate(ctx context.Context, instID, requestedBy, reason string) (runResponse, error) {
	var out runResponse
	body := EngineOpRequest{RequestedBy: requestedBy, Reason: reason}
	return out, c.do(ctx, http.MethodPost, "/v1/instances/"+url.PathEscape(instID)+"/firebird-update/apply", nil, body, &out)
}

// Pprof captures a synchronous CPU/heap/goroutine profile of the agent
// process (cpuSeconds 0-30; 0 = heap+goroutine only). The call lasts about
// cpuSeconds.
func (c *Client) Pprof(ctx context.Context, cpuSeconds int) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.do(ctx, http.MethodPost, "/v1/diagnostics/pprof", nil, map[string]int{"cpu_seconds": cpuSeconds}, &out)
}

// PollOperation polls an async operation to a terminal state, emitting prog
// per tick. It returns populated (op, nil) for BOTH terminal states — check
// op.State — and an error only for transport failures or the ctx deadline
// (op-timeout marker). The first poll is immediate so already-finished
// operations return without waiting an interval.
func (c *Client) PollOperation(ctx context.Context, opID string, interval time.Duration, prog func(float64, string)) (Operation, error) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for i := 0; ; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return Operation{}, fmt.Errorf("fbagent op-timeout: operation %s did not finish before the poll deadline (poll later with fb_agent_operation)", opID)
			case <-tick.C:
			}
		}
		op, err := c.GetOperation(ctx, opID)
		if err != nil {
			if ctx.Err() != nil {
				// The deadline fired mid-poll — report it as a poll
				// timeout, not a transport failure.
				return Operation{}, fmt.Errorf("fbagent op-timeout: operation %s did not finish before the poll deadline (poll later with fb_agent_operation)", opID)
			}
			return Operation{}, err
		}
		switch op.State {
		case "succeeded", "failed":
			return op, nil
		}
		if prog != nil {
			prog(0.5, fmt.Sprintf("operation %s: %s", opID, op.State))
		}
	}
}

// FormatOperation renders an operation document for tool output.
func FormatOperation(op Operation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "operation %s\nkind: %s\nstate: %s\n", op.OperationID, op.Kind, op.State)
	if len(op.Warnings) > 0 {
		fmt.Fprintf(&b, "warnings: %s\n", strings.Join(op.Warnings, "; "))
	}
	if op.Error != nil {
		fmt.Fprintf(&b, "error: %s: %s\n", op.Error.Code, op.Error.Message)
	}
	if id := op.RestartOpID(); id != "" {
		fmt.Fprintf(&b, "nested restart_operation_id: %s (poll with fb_agent_operation)\n", id)
	}
	if op.StatusURL != "" {
		fmt.Fprintf(&b, "status_url: %s\n", op.StatusURL)
	}
	return strings.TrimRight(b.String(), "\n")
}
