// Package uigrid serves fbmcp's result-set grid (phase10_plan.md): typed
// payloads from tabular tools, an in-memory identity-scoped registry with
// TTL, and the single-file HTML bundle rendered inside MCP-Apps chat
// clients. The grid is a pure renderer — no network, no tool calls, no
// persistence: a registry entry dies with the process or at TTL, so the
// chaos harness stays kill-neutral and nothing lands in state.json.
package uigrid

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/identity"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// grid.html is the whole bundle: styles, renderer, CSP meta. One file, no
// build step, no external requests; the payload rides inline as
// window.__GRID_DATA__.
//
//go:embed grid.html
var bundleHTML string

// bundleTemplate is parsed once; the data rides as a JSON literal, never as
// template markup.
var bundleTemplate = template.Must(template.New("grid").Parse(bundleHTML))

// SchemaVersion is the payload contract version (ADR-036), shared with the
// Plamenix grid bundle so either renderer can consume either server's
// payload.
const SchemaVersion = 1

const (
	// CellClip caps one cell's string length in the resource payload. Full
	// values stay in the tool's structuredContent; the grid cell shows the
	// marker and the AI still sees everything.
	CellClip = 256
	// clipMarker is appended to clipped cells.
	clipMarker = "…[truncated]"

	// URIPrefix is the resource-template scheme for grid results.
	URIPrefix = "ui://fbmcp/results/"
)

// Defaults for Options zero values.
const (
	DefaultTTL      = 30 * time.Minute
	DefaultMaxLive  = 32
	DefaultMaxBytes = 2 << 20 // 2 MiB
)

// ErrExpired / ErrForbidden are the registry read failures. Their messages
// lead with the stable code so a resource-read error and any errText-wrapped
// path classify via errdoc (seeds result-expired / result-forbidden).
var (
	ErrExpired   = errors.New("result-expired: grid result unknown or expired (TTL) — re-run the query")
	ErrForbidden = errors.New("result-forbidden: grid result belongs to another identity")
)

// Col describes one result column.
type Col struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Payload is the grid payload contract (ADR-036 schema 1). Cells are typed:
// nil (JSON null), int64/float64/bool, or string. NUMERIC/DECIMAL stay
// strings (the driver scans them as strings); the column type drives
// rendering only.
type Payload struct {
	Schema        int     `json:"schema"`
	Title         string  `json:"title"`
	Columns       []Col   `json:"columns"`
	Rows          [][]any `json:"rows"`
	RowCount      int     `json:"row_count"`
	TotalEstimate *int    `json:"total_estimate,omitempty"`
	Truncated     bool    `json:"truncated,omitempty"`
	ClippedRows   int     `json:"clipped_rows,omitempty"` // dropped by the byte cap, not by max_rows
	ElapsedMS     float64 `json:"elapsed_ms,omitempty"`
	Plan          string  `json:"plan,omitempty"`
	Stats         any     `json:"stats,omitempty"`
	PerTable      any     `json:"per_table_stats,omitempty"`
}

// Options carries the Gridify decorations.
type Options struct {
	Truncated     bool
	ElapsedMS     float64
	Plan          string
	Stats         any
	PerTable      any
	TotalEstimate *int
}

// Gridify assembles the payload from typed cells: clips cell strings to
// CellClip (with marker) and leaves row/byte caps to the registry. A nil or
// empty columns slice yields nil — zero-column results (statements with no
// result set) render nothing and get no ui reference.
func Gridify(title string, cols []Col, rows [][]any, o Options) *Payload {
	if len(cols) == 0 {
		return nil
	}
	p := &Payload{
		Schema:        SchemaVersion,
		Title:         title,
		Columns:       cols,
		Rows:          rows,
		RowCount:      len(rows),
		TotalEstimate: o.TotalEstimate,
		Truncated:     o.Truncated,
		ElapsedMS:     o.ElapsedMS,
		Plan:          o.Plan,
		Stats:         o.Stats,
		PerTable:      o.PerTable,
	}
	for _, r := range rows {
		for i, c := range r {
			if s, ok := c.(string); ok && len(s) > CellClip {
				r[i] = s[:CellClip] + clipMarker
			}
		}
	}
	return p
}

// MapType maps the driver's DatabaseTypeName (+ PrecisionScale) onto the
// grid type vocabulary (ADR-036). Grounded in the IBSurgeon fork of
// nakagami/firebirdsql v0.9.19 (go.mod replace) — xsqlvar.go:
// NUMERIC/DECIMAL arrive as SHORT/LONG/INT64/INT128
// with a negative scale and scan as strings — cells stay strings; the type
// drives alignment only. Unknown names degrade to "unknown" (left-aligned
// string rendering — never wrong).
func MapType(dbTypeName string, scale int64, hasScale bool) string {
	switch strings.ToUpper(strings.TrimSpace(dbTypeName)) {
	case "SHORT", "LONG", "QUAD":
		return "integer"
	case "INT64", "INT128":
		if hasScale && scale < 0 {
			return "decimal"
		}
		return "integer"
	case "DECFLOAT(16)", "DECFLOAT(34)", "DECFIXED":
		return "decimal"
	case "FLOAT", "DOUBLE":
		return "float"
	case "TEXT", "VARYING":
		return "string"
	case "DATE":
		return "date"
	case "TIME":
		return "time"
	case "TIME WITH TIMEZONE", "TIMESTAMP", "TIMESTAMP WITH TIMEZONE":
		return "timestamp"
	case "BOOLEAN":
		return "boolean"
	case "BLOB":
		return "blob"
	default:
		return "unknown"
	}
}

// entry is one stored result.
type entry struct {
	payload  *Payload
	data     []byte // marshaled payload, byte-capped
	identity string
	expiry   time.Time
}

// RegistryOpts are the registry limits (config grid: block).
type RegistryOpts struct {
	Enabled  bool
	TTL      time.Duration // 0 → 30m
	MaxLive  int           // per identity; 0 → 32
	MaxBytes int           // marshaled payload cap; 0 → 2 MiB
	Now      func() time.Time
}

// Registry is the in-memory, identity-scoped result store.
type Registry struct {
	mu    sync.Mutex
	opts  RegistryOpts
	byID  map[string]*entry
	order map[string][]string // identity → ids in insertion order
}

// New builds a registry, filling zero-value limits with the defaults.
func New(opts RegistryOpts) *Registry {
	if opts.TTL <= 0 {
		opts.TTL = DefaultTTL
	}
	if opts.MaxLive <= 0 {
		opts.MaxLive = DefaultMaxLive
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Registry{opts: opts, byID: map[string]*entry{}, order: map[string][]string{}}
}

// Enabled reports whether grid resources are served.
func (r *Registry) Enabled() bool { return r != nil && r.opts.Enabled }

// Put stores a payload under a fresh random id for the identity. It sweeps
// expired entries, byte-caps the payload (dropping trailing rows, never
// columns), and evicts the identity's oldest entry beyond MaxLive.
func (r *Registry) Put(identityName, title string, p *Payload) (string, bool) {
	if !r.Enabled() || p == nil {
		return "", false
	}
	p.Title = title
	data, clipped := marshalCapped(p, r.opts.MaxBytes)
	p.ClippedRows = clipped

	id, err := newID()
	if err != nil {
		return "", false
	}
	now := r.opts.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(now)
	r.byID[id] = &entry{payload: p, data: data, identity: identityName, expiry: now.Add(r.opts.TTL)}
	r.order[identityName] = append(r.order[identityName], id)
	ids := r.order[identityName]
	for len(ids) > r.opts.MaxLive {
		old := ids[0]
		ids = ids[1:]
		delete(r.byID, old)
	}
	r.order[identityName] = ids
	return id, true
}

// Get returns the stored payload bytes for the identity, or the stable-code
// error (ErrExpired / ErrForbidden).
func (r *Registry) Get(identityName, id string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	if !ok || r.opts.Now().After(e.expiry) {
		return nil, ErrExpired
	}
	if e.identity != identityName {
		return nil, ErrForbidden
	}
	return e.data, nil
}

// sweepLocked drops expired entries and their per-identity ordering slots.
// Caller holds mu.
func (r *Registry) sweepLocked(now time.Time) {
	dead := map[string]bool{}
	for id, e := range r.byID {
		if now.After(e.expiry) {
			dead[id] = true
			delete(r.byID, id)
		}
	}
	for who, ids := range r.order {
		kept := ids[:0]
		for _, id := range ids {
			if !dead[id] {
				kept = append(kept, id)
			}
		}
		r.order[who] = kept
	}
}

// marshalCapped marshals the payload and, while over the byte cap, drops
// trailing rows (reporting how many). ClippedRows is set on the payload
// before each marshal so the served bytes carry it. A payload that stays
// over the cap with zero rows still serves — the grid shows the clipped
// banner.
func marshalCapped(p *Payload, maxBytes int) ([]byte, int) {
	dropped := 0
	data, err := json.Marshal(p)
	for err == nil && len(data) > maxBytes && len(p.Rows) > 0 {
		p.Rows = p.Rows[:len(p.Rows)-1]
		p.RowCount = len(p.Rows)
		dropped++
		p.ClippedRows = dropped
		data, err = json.Marshal(p)
	}
	if err != nil {
		return nil, dropped
	}
	return data, dropped
}

// URI is the resource uri for a result id.
func URI(id string) string { return URIPrefix + id }

// MetaUI is the tools/call _meta payload pointing the client at the grid
// resource (go-sdk CallToolResult.Meta is a plain map).
func MetaUI(uri string) map[string]any {
	return map[string]any{"ui": map[string]any{"resourceUri": uri}}
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("grid id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Render assembles the sandboxed page: embedded bundle + the payload
// inlined as window.__GRID_DATA__. The JSON is <, >, & escaped so no
// "</script>" or entity sequence in the data can break out of the script
// element.
func Render(data []byte) string {
	safe := strings.NewReplacer("<", "\\u003c", ">", "\\u003e", "&", "\\u0026").Replace(string(data))
	var b strings.Builder
	if err := bundleTemplate.Execute(&b, safe); err != nil {
		return "<!doctype html><title>grid error</title>"
	}
	return b.String()
}

// RegisterResource serves ui://fbmcp/results/{result_id} on the MCP server.
// The client fetches the resource through its own MCP session, so remote
// (HTTP/SSE) mode needs no extra surface — ADR-022 auth already guards it,
// and the handler enforces identity scoping via the registry. Every served
// read gets one hash-chained audit line (phase10_plan §5 / §11 D1); aud may
// be nil (tests). No-op when the registry is disabled.
func RegisterResource(server *mcp.Server, r *Registry, aud *audit.Logger) {
	if !r.Enabled() {
		return
	}
	server.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: URIPrefix + "{result_id}",
		Name:        "result grid",
		MIMEType:    "text/html",
		Description: "Sortable result-set grid (MCP Apps) for a recent fb_query result; renders in the chat. Fallback: the tool's structuredContent",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		uri := req.Params.URI
		id := strings.TrimPrefix(uri, URIPrefix)
		who := identity.Caller(ctx).Name
		if id == "" || strings.Contains(id, "/") {
			auditResourceRead(aud, who, id, 0, ErrExpired)
			return nil, ErrExpired
		}
		data, err := r.Get(who, id)
		if err != nil {
			auditResourceRead(aud, who, id, 0, err)
			return nil, err
		}
		auditResourceRead(aud, who, id, len(data), nil)
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI: uri, MIMEType: "text/html", Text: Render(data),
		}}}, nil
	})
}

// auditResourceRead writes the §5 one-liner: allow with {result_id, bytes},
// denied (cross-identity) or error (unknown/expired id) otherwise.
func auditResourceRead(aud *audit.Logger, identityName, resultID string, n int, err error) {
	if aud == nil {
		return
	}
	e := audit.Entry{Identity: identityName, Tool: "uigrid.resource", Tier: 0, Decision: "allow"}
	if err != nil {
		e.Decision = "error"
		if errors.Is(err, ErrForbidden) {
			e.Decision = "denied"
		}
		e.Detail = map[string]interface{}{"result_id": resultID, "reason": err.Error()}
	} else {
		e.Detail = map[string]interface{}{"result_id": resultID, "bytes": n}
	}
	aud.Log(e)
}
