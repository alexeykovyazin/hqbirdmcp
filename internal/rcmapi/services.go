package rcmapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// The services of RCM (hqcluster3 docs/rcm.md, "Services", "Switchover and
// rejoin", "Failover"): a logical database on several hosts, whose master
// RCM moves with an operation (switchover, failover, rejoin).

// OperationStep is one step of an operation.
type OperationStep struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	State  string `json:"state"` // pending | active | done | failed
	Detail string `json:"detail,omitempty"`
	// NoReturn: the point of no return is before this step (RCM 2027.5.11+).
	NoReturn  bool       `json:"no_return,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// Operation is a switchover, a failover or a rejoin of a service
// (GET /v1/operations/{id}).
type Operation struct {
	ID           string          `json:"operation_id"`
	Kind         string          `json:"kind"`
	ServiceID    string          `json:"service_id"`
	Status       string          `json:"status"` // running | ok | failed | cancelled
	Actor        string          `json:"actor,omitempty"`
	FromHost     string          `json:"from_host,omitempty"`
	ToHost       string          `json:"to_host,omitempty"`
	Epoch        int64           `json:"epoch,omitempty"`
	Steps        []OperationStep `json:"steps"`
	PastNoReturn bool            `json:"past_no_return,omitempty"`
	Error        string          `json:"error,omitempty"`
	Warnings     []string        `json:"warnings,omitempty"`
}

// Running reports whether the operation has not ended.
func (o Operation) Running() bool { return o.Status == "running" || o.Status == "" }

// ActiveStep is the step that runs now ("" when none does).
func (o Operation) ActiveStep() string {
	for _, s := range o.Steps {
		if s.State == "active" {
			return s.ID
		}
	}
	return ""
}

// Progress is the share of the steps that are done.
func (o Operation) Progress() float64 {
	if len(o.Steps) == 0 {
		return 0
	}
	done := 0
	for _, s := range o.Steps {
		if s.State == "done" {
			done++
		}
	}
	return float64(done) / float64(len(o.Steps))
}

// Problem is one reason a check or a start of an operation is refused.
type Problem struct {
	Code    string `json:"code"`
	NodeID  string `json:"node_id,omitempty"`
	Message string `json:"message"`
}

// Refusal is what a refused start carries beside the error: the problems of
// the checks, and the operation that runs on the service (RCM 2027.5.11+).
type Refusal struct {
	Problems    []Problem       `json:"problems"`
	OperationID string          `json:"operation_id,omitempty"`
	DataLoss    json.RawMessage `json:"data_loss,omitempty"`
}

// RefusalOf reads the problems of an RCM error; ok is false when the error
// is not an RCM answer or carries none.
func RefusalOf(err error) (Refusal, bool) {
	var e *Error
	if !errors.As(err, &e) || len(e.Body) == 0 {
		return Refusal{}, false
	}
	var r Refusal
	if json.Unmarshal(e.Body, &r) != nil {
		return Refusal{}, false
	}
	return r, len(r.Problems) > 0 || r.OperationID != "" || len(r.DataLoss) > 0
}

// Operation reads one operation.
func (c *Client) Operation(ctx context.Context, id string) (Operation, json.RawMessage, error) {
	raw, err := c.Do(ctx, http.MethodGet, "/v1/operations/"+PathEscape(id), nil)
	if err != nil {
		return Operation{}, nil, err
	}
	var op Operation
	if err := json.Unmarshal(raw, &op); err != nil {
		return Operation{}, raw, err
	}
	return op, raw, nil
}

// WaitOperation polls an operation until it ends. prog sees it at every
// poll while it runs.
func (c *Client) WaitOperation(ctx context.Context, id string, every time.Duration, prog func(Operation)) (Operation, error) {
	for {
		op, _, err := c.Operation(ctx, id)
		if err != nil {
			return Operation{}, err
		}
		if !op.Running() {
			return op, nil
		}
		if prog != nil {
			prog(op)
		}
		select {
		case <-ctx.Done():
			return op, fmt.Errorf("operation %s is still running after the wait budget (step %q); follow it with fb_cluster_service view=operation id=%s", id, op.ActiveStep(), id)
		case <-time.After(every):
		}
	}
}
