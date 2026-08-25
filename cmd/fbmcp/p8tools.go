// Phase 8 surfaces (phase8_plan D3.3): the playbooks under prompts/ served
// as MCP prompts (the directory is the source of truth — served dynamically,
// so it cannot drift), and read-only kernel resources (registry, policy,
// config summary, audit head). Resources expose only non-secret fields:
// the config's secret_* fields hold environment-variable NAMES by design,
// and the builders here never touch secret values at all.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/state"
)

func registerSurfaces(server *mcp.Server, handle *config.Handle, st *state.Store) {
	if dir := findPromptsDir(); dir != "" {
		registerPlaybookPrompts(server, dir)
	}
	registerKernelResources(server, handle, st)
}

// findPromptsDir locates fbmcp/prompts relative to the working directory or
// the executable (dist\..\prompts in service mode).
func findPromptsDir() string {
	exe, _ := os.Executable()
	candidates := []string{"prompts", filepath.Join("..", "..", "prompts")}
	if exe != "" {
		d := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(d, "..", "prompts"),
			filepath.Join(d, "..", "..", "prompts"))
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			return c
		}
	}
	return ""
}

func registerPlaybookPrompts(server *mcp.Server, dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		path := filepath.Join(dir, e.Name())
		server.AddPrompt(&mcp.Prompt{Name: name, Description: "fbmcp operator playbook: " + name},
			func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				b, err := os.ReadFile(path)
				if err != nil {
					return nil, err
				}
				return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{
					Role: "user", Content: &mcp.TextContent{Text: string(b)},
				}}}, nil
			})
	}
}

// registryResource lists databases and instances by registry id — never a
// path the client should free-type, and never a secret value.
func registryResource(cfg *config.Config) string {
	type dbRow struct {
		ID, Instance, ROUser, AdminUser string
	}
	type instRow struct {
		ID, Addr, Version, Service string
	}
	var dbs []dbRow
	for _, d := range cfg.Databases {
		dbs = append(dbs, dbRow{ID: d.ID, Instance: d.Instance, ROUser: d.ROUser, AdminUser: d.AdminUser})
	}
	var insts []instRow
	for _, i := range cfg.Instances {
		insts = append(insts, instRow{ID: i.ID, Addr: i.Addr, Version: i.Version, Service: i.Service})
	}
	b, _ := json.MarshalIndent(map[string]any{"databases": dbs, "instances": insts}, "", " ")
	return string(b)
}

// policyResource dumps the effective tier table (source of truth: toolMeta).
func policyResource() string {
	var b strings.Builder
	for _, m := range toolMeta {
		fmt.Fprintf(&b, "%s tier=%d scope=%s", m.Name, m.Tier, m.Scope)
		if m.MinFB != "" {
			fmt.Fprintf(&b, " min_fb=%s", m.MinFB)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// configResource is an explicit allowlist of non-secret config fields.
func configResource(cfg *config.Config) string {
	ids := map[string]int{}
	for _, i := range cfg.Identities {
		ids[i.Name] = i.MaxTier
	}
	b, _ := json.MarshalIndent(map[string]any{
		"state_dir":                 cfg.State.Dir,
		"listen":                    cfg.Listen,
		"tls":                       cfg.TLS.Cert != "" && cfg.TLS.Key != "",
		"local_max_tier":            cfg.LocalMaxTierOrDefault(),
		"identity_names_and_tiers":  ids,
		"notify_webhook_configured": cfg.Notify.WebhookURL != "",
		"note":                      "secret values live in environment variables; this resource names none of them",
	}, "", " ")
	return string(b)
}

// auditHeadResource surfaces the chain head sidecar (count + last hash).
func auditHeadResource(cfg *config.Config) string {
	b, err := os.ReadFile(filepath.Join(cfg.State.Dir, "audit.jsonl.head"))
	if err != nil {
		return `{"error": "audit head sidecar not found"}`
	}
	return string(b)
}

func registerKernelResources(server *mcp.Server, handle *config.Handle, st *state.Store) {
	_ = st // reserved: schedule/job counts once resources grow
	add := func(uri, name, desc string, build func() string) {
		server.AddResource(&mcp.Resource{URI: uri, Name: name, Description: desc, MIMEType: "text/plain"},
			func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
					URI: uri, MIMEType: "text/plain", Text: build(),
				}}}, nil
			})
	}
	add("fbmcp://registry", "registry", "Registered databases and instances (ids only; no paths, no secrets)", func() string {
		return registryResource(handle.Current())
	})
	add("fbmcp://policy", "policy", "Effective tool tier table", policyResource)
	add("fbmcp://config", "config", "Non-secret config summary", func() string {
		return configResource(handle.Current())
	})
	add("fbmcp://audit/head", "audit head", "Audit chain head sidecar (entry count + last hash)", func() string {
		return auditHeadResource(handle.Current())
	})
}
