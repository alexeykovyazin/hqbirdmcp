package rcmapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ProbeHost is one host of POST /v1/admin/pair/probe (hqcluster3
// clustersrv.ProbeHost; only the fields fbmcp shows).
type ProbeHost struct {
	Input           string            `json:"input"`
	Host            string            `json:"host"`
	Port            int               `json:"port"`
	Address         string            `json:"address,omitempty"`
	Hostname        string            `json:"hostname,omitempty"`
	NodeName        string            `json:"node_name,omitempty"`
	Platform        string            `json:"platform,omitempty"`
	FBAgentVersion  string            `json:"fbagent_version,omitempty"`
	FirebirdVersion string            `json:"firebird_version,omitempty"`
	Fingerprint     string            `json:"fingerprint,omitempty"`
	PairState       string            `json:"pair_state,omitempty"`
	RCMHost         bool              `json:"rcm_host"`
	Roles           []string          `json:"roles"`
	MemberIDs       map[string]string `json:"member_ids,omitempty"`
	State           string            `json:"state"`
	Code            string            `json:"code,omitempty"`
	Error           string            `json:"error,omitempty"`
}

// ProbeAnswer is the answer of the probe.
type ProbeAnswer struct {
	ClusterID string      `json:"cluster_id"`
	RCMURL    string      `json:"rcm_url"`
	Seats     int         `json:"seats"`
	Hosts     []ProbeHost `json:"hosts"`
	OK        bool        `json:"ok"`
}

// Host states of a probe and a link (clustersrv).
const (
	HostFree    = "free"
	HostMember  = "member"
	HostError   = "error"
	HostRunning = "running"
)

// Link job states.
const (
	LinkRunning = "running"
	LinkDone    = "done"
	LinkFailed  = "failed"
)

// LinkHostIn is one host of POST /v1/admin/pair/link.
type LinkHostIn struct {
	Host        string `json:"host"`
	Fingerprint string `json:"fingerprint"`
}

// LinkRequest is the body of POST /v1/admin/pair/link.
type LinkRequest struct {
	Hosts  []LinkHostIn `json:"hosts"`
	Master string       `json:"master"`
	RCMURL string       `json:"rcm_url"`
}

// LinkHost is one host of a link job.
type LinkHost struct {
	ProbeHost
	Role     string `json:"role"`
	MemberID string `json:"member_id,omitempty"`
	Service  string `json:"service,omitempty"`
}

// LinkJob is GET /v1/admin/pair/link/{id}.
type LinkJob struct {
	ID       string     `json:"id"`
	State    string     `json:"state"`
	Step     string     `json:"step"`
	Code     string     `json:"code,omitempty"`
	Error    string     `json:"error,omitempty"`
	Errors   []string   `json:"errors,omitempty"`
	Master   string     `json:"master"`
	RCMURL   string     `json:"rcm_url"`
	Revision int64      `json:"revision,omitempty"`
	Hosts    []LinkHost `json:"hosts"`
}

// Probe asks every host's pairing port for its facts.
func (c *Client) Probe(ctx context.Context, hosts []string) (ProbeAnswer, error) {
	var p ProbeAnswer
	raw, err := c.Do(ctx, http.MethodPost, "/v1/admin/pair/probe", map[string]any{"hosts": hosts})
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(raw, &p)
}

// Link starts the link job.
func (c *Client) Link(ctx context.Context, req LinkRequest) (LinkJob, error) {
	var j LinkJob
	raw, err := c.Do(ctx, http.MethodPost, "/v1/admin/pair/link", req)
	if err != nil {
		return j, err
	}
	return j, json.Unmarshal(raw, &j)
}

// LinkStatus reads a link job ("latest": the last one).
func (c *Client) LinkStatus(ctx context.Context, id string) (LinkJob, error) {
	var j LinkJob
	raw, err := c.Do(ctx, http.MethodGet, "/v1/admin/pair/link/"+PathEscape(id), nil)
	if err != nil {
		return j, err
	}
	return j, json.Unmarshal(raw, &j)
}

// WaitLink polls the link job until it is no longer running or ctx ends.
func (c *Client) WaitLink(ctx context.Context, job LinkJob, every time.Duration, prog func(LinkJob)) (LinkJob, error) {
	for job.State == LinkRunning {
		if prog != nil {
			prog(job)
		}
		select {
		case <-ctx.Done():
			return job, fmt.Errorf("link job %s still runs after the wait budget (step %q); follow it with fb_cluster_status view=link", job.ID, job.Step)
		case <-time.After(every):
		}
		next, err := c.LinkStatus(ctx, job.ID)
		if err != nil {
			return job, err
		}
		job = next
	}
	return job, nil
}

// Command is the part of GET /v1/commands/{id} fbmcp waits on.
type Command struct {
	ID     string          `json:"id"`
	State  string          `json:"state"`
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// Done reports a final state of a node command.
func (c Command) Done() bool {
	switch c.State {
	case "succeeded", "failed", "timeout", "cancelled", "expired":
		return true
	}
	return false
}

// WaitCommand polls a node command until it ends. It returns the last raw
// document too: the caller prints it.
func (c *Client) WaitCommand(ctx context.Context, id string, every time.Duration) (Command, json.RawMessage, error) {
	for {
		raw, err := c.Do(ctx, http.MethodGet, "/v1/commands/"+PathEscape(id), nil)
		if err != nil {
			return Command{}, nil, err
		}
		var cmd Command
		if err := json.Unmarshal(raw, &cmd); err != nil {
			return Command{}, raw, err
		}
		if cmd.Done() {
			return cmd, raw, nil
		}
		select {
		case <-ctx.Done():
			return cmd, raw, fmt.Errorf("command %s is still %s after the wait budget; follow it with fb_cluster_status view=command id=%s", id, cmd.State, id)
		case <-time.After(every):
		}
	}
}

// Job is the part of a reinit or verify job fbmcp waits on (store.ReinitJob,
// the verify job): status running → ok | failed | …
type Job struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Phase   string `json:"phase,omitempty"`
	Percent int    `json:"percent,omitempty"`
	Error   string `json:"error,omitempty"`
	Code    string `json:"code,omitempty"`
}

// WaitJob polls GET path/{id} (/v1/reinit or /v1/verify) until the status
// is no longer "running" (or empty).
func (c *Client) WaitJob(ctx context.Context, path, id string, every time.Duration, prog func(Job)) (Job, json.RawMessage, error) {
	for {
		raw, err := c.Do(ctx, http.MethodGet, path+"/"+PathEscape(id), nil)
		if err != nil {
			return Job{}, nil, err
		}
		var j Job
		if err := json.Unmarshal(raw, &j); err != nil {
			return Job{}, raw, err
		}
		if j.Status != "" && j.Status != "running" {
			return j, raw, nil
		}
		if prog != nil {
			prog(j)
		}
		select {
		case <-ctx.Done():
			return j, raw, fmt.Errorf("job %s is still running after the wait budget (phase %q)", id, j.Phase)
		case <-time.After(every):
		}
	}
}
