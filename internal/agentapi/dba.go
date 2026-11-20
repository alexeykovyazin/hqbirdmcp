package agentapi

// The DBA operations of fbagent (/v1/dba/..., fbagent docs
// DBA_ASSISTANT_PLAN.ru.md §3): one-time actions on the agent's host. Every
// action has a check that changes nothing and shows the exact commands
// (passwords masked); a run of a disruptive, destructive or critical
// operation needs the confirm token of its check.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// DBA operation classes, as the agent sends them.
const (
	DBAClassRead        = "read"
	DBAClassNormal      = "normal"
	DBAClassDisruptive  = "disruptive"
	DBAClassDestructive = "destructive"
	DBAClassCritical    = "critical"
)

// DBA run states.
const (
	DBAStateRunning   = "running"
	DBAStateDone      = "done"
	DBAStateFailed    = "failed"
	DBAStateCancelled = "cancelled"
)

// DBARequest is the body of check and run.
type DBARequest struct {
	// Database is the full local path of the database as the agent knows
	// it. Empty for an operation of the server scope.
	Database     string         `json:"database"`
	Params       map[string]any `json:"params,omitempty"`
	RequestedBy  string         `json:"requested_by,omitempty"`
	Reason       string         `json:"reason,omitempty"`
	RequestID    string         `json:"request_id,omitempty"`
	ConfirmToken string         `json:"confirm_token,omitempty"`
	ConfirmName  string         `json:"confirm_name,omitempty"`
}

// DBACheckItem is one line of a check.
type DBACheckItem struct {
	Level string `json:"level"` // ok | warn | block
	Code  string `json:"code"`
	Text  string `json:"text"`
}

// DBACommand is one step of an operation: the full command (password
// masked) or a line that describes a step no tool runs.
type DBACommand struct {
	Phase      string `json:"phase"`
	Binary     string `json:"binary,omitempty"`
	Command    string `json:"command"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	OK         *bool  `json:"ok,omitempty"`
	Error      string `json:"error,omitempty"`
}

// DBACheck is the answer of POST /v1/dba/ops/{op}/check.
type DBACheck struct {
	Op           string          `json:"op"`
	Class        string          `json:"class"`
	Database     string          `json:"database,omitempty"`
	Params       map[string]any  `json:"params"`
	Checks       []DBACheckItem  `json:"checks"`
	Blocked      bool            `json:"blocked"`
	Commands     []DBACommand    `json:"commands"`
	Details      json.RawMessage `json:"details,omitempty"`
	ConfirmToken string          `json:"confirm_token,omitempty"`
	ExpiresAt    string          `json:"expires_at,omitempty"`
	ConfirmName  string          `json:"confirm_name,omitempty"`
}

// DBARun is one run of an operation.
type DBARun struct {
	RunID        string          `json:"run_id"`
	Op           string          `json:"op"`
	Class        string          `json:"class"`
	Database     string          `json:"database,omitempty"`
	Params       map[string]any  `json:"params"`
	Source       string          `json:"source"`
	RequestedBy  string          `json:"requested_by,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	RequestID    string          `json:"request_id,omitempty"`
	State        string          `json:"state"`
	StateReason  string          `json:"state_reason,omitempty"`
	StateMessage string          `json:"state_message,omitempty"`
	Step         string          `json:"step,omitempty"`
	StepIndex    int             `json:"step_index,omitempty"`
	StepCount    int             `json:"step_count"`
	Commands     []DBACommand    `json:"commands"`
	Warnings     []string        `json:"warnings"`
	Result       json.RawMessage `json:"result,omitempty"`
	OutputBytes  int64           `json:"output_bytes"`
	CreatedAt    string          `json:"created_at"`
	FinishedAt   string          `json:"finished_at,omitempty"`
}

// Final reports whether the run has ended.
func (r DBARun) Final() bool { return r.State != "" && r.State != DBAStateRunning }

// DBARunStart is the 202 of POST /v1/dba/ops/{op}/run.
type DBARunStart struct {
	RunID  string `json:"run_id"`
	RunURL string `json:"run_url"`
	Run    DBARun `json:"run"`
}

// DBARefusal is a 409 of check or run: the agent will not do it now. Reason
// is the agent's code: blocked, confirm_required, confirm_stale,
// confirm_name_required, busy, disabled, not_licensed, too_many_runs.
type DBARefusal struct {
	Code    string
	Message string
	Reason  string
	Checks  []DBACheckItem
}

func (e *DBARefusal) Error() string {
	s := fmt.Sprintf("fbagent conflict (409, %s): %s", e.Code, e.Message)
	if e.Reason != "" {
		s += " (reason " + e.Reason + ")"
	}
	return s
}

// dbaRefusal reads a 409 body. It returns nil when the body is not the
// refusal shape (a plain conflict, e.g. a cancel of an ended run).
func dbaRefusal(data []byte) *DBARefusal {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Reason string         `json:"reason"`
		Checks []DBACheckItem `json:"checks"`
	}
	if json.Unmarshal(data, &env) != nil || (env.Reason == "" && len(env.Checks) == 0) {
		return nil
	}
	return &DBARefusal{Code: env.Error.Code, Message: env.Error.Message, Reason: env.Reason, Checks: env.Checks}
}

// doDBA is do for the check and run routes: a 409 with the refusal shape
// comes back as *DBARefusal.
func (c *Client) doDBA(ctx context.Context, path string, body, out any) error {
	status, _, data, err := c.send(ctx, http.MethodPost, path, nil, body)
	if err != nil {
		return err
	}
	if status == http.StatusConflict {
		if r := dbaRefusal(data); r != nil {
			return r
		}
	}
	if status >= 300 {
		return apiError(status, data)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("fbagent service-error: POST %s: response is not valid JSON: %w", path, err)
	}
	return nil
}

// DBATools lists the Firebird tools (gbak, nbackup, gstat, gfix...) that run
// on the host now. It reads the process table only.
func (c *Client) DBATools(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.do(ctx, http.MethodGet, "/v1/dba/tools", nil, nil, &out)
}

// DBAOps is the catalog of operations. With db, each operation carries its
// commands with the default parameters for that database.
func (c *Client) DBAOps(ctx context.Context, db string) (json.RawMessage, error) {
	q := url.Values{}
	if db != "" {
		q.Set("db", db)
	}
	var out json.RawMessage
	return out, c.do(ctx, http.MethodGet, "/v1/dba/ops", q, nil, &out)
}

// DBAOpTitle returns the title of one operation from the catalog, or "" when
// the catalog has no such operation.
func (c *Client) DBAOpTitle(ctx context.Context, op string) (string, error) {
	var out struct {
		Ops []struct {
			ID    string `json:"op"`
			Title string `json:"title"`
		} `json:"ops"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/dba/ops", nil, nil, &out); err != nil {
		return "", err
	}
	for _, o := range out.Ops {
		if o.ID == op {
			return o.Title, nil
		}
	}
	return "", nil
}

// DBADatabase is the state of one database, read without an attach.
func (c *Client) DBADatabase(ctx context.Context, db string) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.do(ctx, http.MethodGet, "/v1/dba/database", url.Values{"db": {db}}, nil, &out)
}

// DBATables lists the user tables of a database. The agent attaches to it.
func (c *Client) DBATables(ctx context.Context, db string) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.do(ctx, http.MethodPost, "/v1/dba/tables", nil, map[string]string{"database": db}, &out)
}

// DBACheck runs the check of an operation. It changes nothing. A refusal is
// a *DBARefusal.
func (c *Client) DBACheck(ctx context.Context, op string, req DBARequest) (DBACheck, error) {
	var out DBACheck
	return out, c.doDBA(ctx, "/v1/dba/ops/"+url.PathEscape(op)+"/check", req, &out)
}

// DBARun starts an operation. A refusal is a *DBARefusal.
func (c *Client) DBARun(ctx context.Context, op string, req DBARequest) (DBARunStart, error) {
	var out DBARunStart
	return out, c.doDBA(ctx, "/v1/dba/ops/"+url.PathEscape(op)+"/run", req, &out)
}

// DBARunFilter selects runs for GetDBARuns.
type DBARunFilter struct {
	Active   bool
	Op       string
	Database string
	Limit    int
}

// GetDBARuns lists runs, newest first.
func (c *Client) GetDBARuns(ctx context.Context, f DBARunFilter) (json.RawMessage, error) {
	q := url.Values{}
	if f.Active {
		q.Set("active", "1")
	}
	if f.Op != "" {
		q.Set("op", f.Op)
	}
	if f.Database != "" {
		q.Set("db", f.Database)
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	var out json.RawMessage
	return out, c.do(ctx, http.MethodGet, "/v1/dba/runs", q, nil, &out)
}

// GetDBARun reads one run, raw and decoded.
func (c *Client) GetDBARun(ctx context.Context, runID string) (DBARun, json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/v1/dba/runs/"+url.PathEscape(runID), nil, nil, &raw); err != nil {
		return DBARun{}, nil, err
	}
	var run DBARun
	if err := json.Unmarshal(raw, &run); err != nil {
		return DBARun{}, raw, fmt.Errorf("fbagent service-error: run %s: %w", runID, err)
	}
	return run, raw, nil
}

// CancelDBARun cancels a running run (409 once it has ended or when the
// operation cannot be stopped).
func (c *Client) CancelDBARun(ctx context.Context, runID string) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.do(ctx, http.MethodDelete, "/v1/dba/runs/"+url.PathEscape(runID), nil, nil, &out)
}

// DBAOutput is what the tools of a run printed.
type DBAOutput struct {
	Text string
	// Truncated: the agent cut the view (X-Output-Truncated: 1, 2 MB).
	Truncated bool
	// Bytes is the size of the whole output file (X-Output-Bytes), -1 when
	// the agent did not say.
	Bytes int64
}

// GetDBAOutput reads the output of a run as text: the start of it, or with
// tail > 0 the last tail lines.
func (c *Client) GetDBAOutput(ctx context.Context, runID string, tail int) (DBAOutput, error) {
	q := url.Values{}
	if tail > 0 {
		q.Set("tail", strconv.Itoa(tail))
	}
	path := "/v1/dba/runs/" + url.PathEscape(runID) + "/output"
	status, h, data, err := c.send(ctx, http.MethodGet, path, q, nil)
	if err != nil {
		return DBAOutput{}, err
	}
	if status >= 300 {
		return DBAOutput{}, apiError(status, data)
	}
	out := DBAOutput{Text: string(data), Truncated: h.Get("X-Output-Truncated") == "1", Bytes: -1}
	if n, err := strconv.ParseInt(strings.TrimSpace(h.Get("X-Output-Bytes")), 10, 64); err == nil {
		out.Bytes = n
	}
	return out, nil
}
