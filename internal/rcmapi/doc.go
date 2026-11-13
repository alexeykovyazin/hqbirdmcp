package rcmapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
)

// The cluster document of RCM (hqcluster3 clustercontract.Cluster): the
// topology an operator edits. fbmcp changes only members and links and
// sends every other field back as it came, so a field it does not know
// survives the PUT.

// RoleRCM is the member that runs RCM; its agent signs the cluster's
// certificates and cannot leave the cluster.
const RoleRCM = "rcm"

// DocMember is one member of the cluster document.
type DocMember struct {
	MemberID   string   `json:"member_id"`
	AgentID    string   `json:"agent_id"`
	Role       string   `json:"role"`
	Addresses  []string `json:"addresses"`
	ListenPort int      `json:"listen_port"`
	raw        json.RawMessage
}

// DocLink is a link of the document: from a master to a replica.
type DocLink struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// ClusterDoc is GET /v1/admin/clusters/{id}.
type ClusterDoc struct {
	ClusterID string
	Revision  int64
	Members   []DocMember
	Links     []DocLink
	raw       map[string]json.RawMessage
}

// ParseClusterDoc reads a cluster document.
func ParseClusterDoc(b []byte) (ClusterDoc, error) {
	var d ClusterDoc
	if err := json.Unmarshal(b, &d.raw); err != nil {
		return d, fmt.Errorf("rcm cluster document: %w", err)
	}
	var head struct {
		ClusterID string            `json:"cluster_id"`
		Revision  int64             `json:"revision"`
		Members   []json.RawMessage `json:"members"`
		Links     []DocLink         `json:"links"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return d, fmt.Errorf("rcm cluster document: %w", err)
	}
	d.ClusterID, d.Revision, d.Links = head.ClusterID, head.Revision, head.Links
	for _, m := range head.Members {
		var dm DocMember
		if err := json.Unmarshal(m, &dm); err != nil {
			return d, fmt.Errorf("rcm cluster document member: %w", err)
		}
		dm.raw = m
		d.Members = append(d.Members, dm)
	}
	return d, nil
}

// Member returns the member with this id.
func (d ClusterDoc) Member(id string) (DocMember, bool) {
	for _, m := range d.Members {
		if m.MemberID == id {
			return m, true
		}
	}
	return DocMember{}, false
}

// AgentMembers are the members of one agent, in document order.
func (d ClusterDoc) AgentMembers(agentID string) []DocMember {
	var out []DocMember
	for _, m := range d.Members {
		if m.AgentID == agentID {
			out = append(out, m)
		}
	}
	return out
}

// Without is the document without these members and without every link
// that starts or ends at one of them, as the RCM Cluster page removes a
// member. It returns the new document and the links it took out.
func (d ClusterDoc) Without(ids map[string]bool) (json.RawMessage, []DocLink, error) {
	members := []json.RawMessage{}
	for _, m := range d.Members {
		if !ids[m.MemberID] {
			members = append(members, m.raw)
		}
	}
	links := []DocLink{}
	var gone []DocLink
	for _, l := range d.Links {
		if ids[l.From] || ids[l.To] {
			gone = append(gone, l)
			continue
		}
		links = append(links, l)
	}
	out := make(map[string]json.RawMessage, len(d.raw))
	for k, v := range d.raw {
		out[k] = v
	}
	mb, err := json.Marshal(members)
	if err != nil {
		return nil, nil, err
	}
	lb, err := json.Marshal(links)
	if err != nil {
		return nil, nil, err
	}
	out["members"], out["links"] = mb, lb
	b, err := json.Marshal(out)
	return b, gone, err
}

// ClusterSummary is one row of GET /v1/admin/clusters.
type ClusterSummary struct {
	ClusterID string `json:"cluster_id"`
	Revision  int64  `json:"revision"`
	Members   int    `json:"members"`
}

// TheCluster is the one cluster of this RCM (an RCM serves one cluster).
func (c *Client) TheCluster(ctx context.Context) (ClusterDoc, error) {
	raw, err := c.Do(ctx, http.MethodGet, "/v1/admin/clusters", nil)
	if err != nil {
		return ClusterDoc{}, err
	}
	var list struct {
		Clusters []ClusterSummary `json:"clusters"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return ClusterDoc{}, fmt.Errorf("rcm clusters: %w", err)
	}
	switch len(list.Clusters) {
	case 0:
		return ClusterDoc{}, errors.New("this RCM has no cluster")
	case 1:
	default:
		ids := make([]string, 0, len(list.Clusters))
		for _, s := range list.Clusters {
			ids = append(ids, s.ClusterID)
		}
		sort.Strings(ids)
		return ClusterDoc{}, fmt.Errorf("this RCM lists %d clusters (%v); fbmcp works with one", len(ids), ids)
	}
	raw, err = c.Do(ctx, http.MethodGet, "/v1/admin/clusters/"+PathEscape(list.Clusters[0].ClusterID), nil)
	if err != nil {
		return ClusterDoc{}, err
	}
	return ParseClusterDoc(raw)
}

// Validation is POST /v1/admin/clusters/{id}/validate.
type Validation struct {
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
}

// ValidateCluster checks a document as a PUT would, without storing it.
func (c *Client) ValidateCluster(ctx context.Context, id string, doc json.RawMessage) (Validation, error) {
	var v Validation
	raw, err := c.Do(ctx, http.MethodPost, "/v1/admin/clusters/"+PathEscape(id)+"/validate", doc)
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("rcm validate: %w", err)
	}
	return v, nil
}

// ReplaceCluster stores a new document; rev is the revision it was made
// against (RCM refuses 409 conflict when the document changed since).
func (c *Client) ReplaceCluster(ctx context.Context, id string, rev int64, doc json.RawMessage) (ClusterDoc, error) {
	h := http.Header{}
	h.Set("If-Match", `"`+strconv.FormatInt(rev, 10)+`"`)
	raw, err := c.DoHeader(ctx, http.MethodPut, "/v1/admin/clusters/"+PathEscape(id), doc, h)
	if err != nil {
		return ClusterDoc{}, err
	}
	return ParseClusterDoc(raw)
}

// RevokeAgent revokes an agent that has no members left: its certificates
// stop working, and a host it ran on can be linked again.
func (c *Client) RevokeAgent(ctx context.Context, agentID string) error {
	_, err := c.Do(ctx, http.MethodPost, "/v1/admin/agents/"+PathEscape(agentID)+"/revoke", nil)
	return err
}

// Agent is one row of GET /v1/admin/agents (the fields fbmcp shows).
type Agent struct {
	AgentID  string   `json:"agent_id"`
	Hostname string   `json:"hostname"`
	Revoked  bool     `json:"revoked"`
	Members  []string `json:"members"`
}

// Agents lists the agents RCM knows.
func (c *Client) Agents(ctx context.Context) ([]Agent, error) {
	raw, err := c.Do(ctx, http.MethodGet, "/v1/admin/agents", nil)
	if err != nil {
		return nil, err
	}
	var list struct {
		Agents []Agent `json:"agents"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("rcm agents: %w", err)
	}
	return list.Agents, nil
}
