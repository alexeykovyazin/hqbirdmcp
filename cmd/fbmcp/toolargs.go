// Argument specs for the generic gated tools (UX review W1.1 / W1.4).
//
// The generic gate path (registerToolEx) takes {db, mode, args}. Before
// this file the args object was untyped: the LLM learned the keys only
// from the impact text after it had created a pending action, and the
// executors read missing or mistyped keys as zero values (a missing
// fb_set_forcewrite "on" switched Forced Writes OFF). The specs below are
// checked BEFORE gate.Request, so a bad call never reaches the approver,
// and they render the "args:" part of each tool description.
package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// argSpec describes one key of a gated tool's args object.
type argSpec struct {
	Name     string
	Kind     string // bool | int | string | text (string; numbers/bools accepted as text) | ints | json
	Required bool
	Enum     []string // allowed values for string kinds (case-insensitive)
	Min, Max int      // int range when Max > Min
	Secret   bool     // value is never shown to the approver or stored in impact text
	Doc      string
}

// toolArgs is the args contract of one gated tool.
type toolArgs struct {
	Args  []argSpec
	OneOf [][]string // each group: at least one of these keys is required
	Note  string     // extra description sentence (when to use another tool)
}

var agentArgSpec = argSpec{Name: "agent", Kind: "string", Doc: "agent id from fbmcp.yaml; optional with one agent"}
var reasonArgSpec = argSpec{Name: "reason", Kind: "string", Doc: "recorded by the agent"}

// gatedArgSpecs covers every tool registered through registerTool /
// registerToolEx (TestGatedToolsHaveArgSpecs keeps it complete).
var gatedArgSpecs = map[string]toolArgs{
	"fb_backup_start": {Args: []argSpec{
		{Name: "parallel_workers", Kind: "int", Min: 1, Max: 64, Doc: "HQbird / Firebird 5 multi-thread backup"},
	}, Note: "Then fb_restore_test marks it verified. fbagent backup jobs: fb_agent_job_run."},
	"fb_backup_nbackup": {Args: []argSpec{
		{Name: "level", Kind: "int", Min: 0, Max: 2, Doc: "nbackup level, default 0; level N needs a level N-1 backup in the catalog"},
	}},
	"fb_restore_test": {Args: []argSpec{
		{Name: "parallel_workers", Kind: "int", Min: 1, Max: 64, Doc: "multi-thread index creation (HQbird / Firebird 5)"},
		{Name: "no_matviews", Kind: "bool", Doc: "skip materialized-view refresh (Firebird 5+)"},
		{Name: "keep_verify", Kind: "bool", Doc: "keep the restored verify copy for a follow-up diff"},
	}},
	"fb_validate":         {},
	"fb_sweep":            {Note: "fbagent sweep job: fb_agent_job_run."},
	"fb_set_forcewrite":   {Args: []argSpec{{Name: "on", Kind: "bool", Required: true, Doc: "true = Forced Writes ON, false = OFF"}}},
	"fb_set_readonly":     {Args: []argSpec{{Name: "readonly", Kind: "bool", Required: true, Doc: "true = READ ONLY, false = READ WRITE"}}},
	"fb_set_page_buffers": {Args: []argSpec{{Name: "buffers", Kind: "int", Required: true, Min: 1, Max: 100000, Doc: "page buffers stored in the database header"}}},
	"fb_trace_start": {Args: []argSpec{
		{Name: "template", Kind: "string", Required: true, Enum: []string{"audit-lite", "performance", "errors"}},
	}, Note: "Stop it with fb_trace_stop; list with fb_trace_list."},
	"fb_trace_stop": {Args: []argSpec{{Name: "session_id", Kind: "string", Required: true, Doc: "the t… id printed by fb_trace_start"}}},
	"fb_restore_replace": {Args: []argSpec{
		{Name: "no_matviews", Kind: "bool", Doc: "skip materialized-view refresh (Firebird 5+)"},
	}, Note: "Needs fb_window_open and a verified backup < 24 h. To only check a backup: fb_restore_test."},
	"fb_index_rebuild": {Args: []argSpec{
		{Name: "index", Kind: "string", Doc: "index name"},
		{Name: "action", Kind: "string", Enum: []string{"rebuild", "statistics"}, Doc: "default rebuild"},
		{Name: "advisory_id", Kind: "string", Doc: "adv… id from fb_index_advice (instead of index)"},
	}, OneOf: [][]string{{"index", "advisory_id"}}},
	"fb_index_drop": {Args: []argSpec{
		{Name: "index", Kind: "string", Doc: "index name"},
		{Name: "advisory_id", Kind: "string", Doc: "adv… id from fb_index_advice (instead of index)"},
	}, OneOf: [][]string{{"index", "advisory_id"}}},
	"fb_user_create": {Args: []argSpec{
		{Name: "user", Kind: "string", Required: true},
		{Name: "password", Kind: "string", Required: true, Secret: true, Doc: "never shown or audited"},
	}},
	"fb_user_drop":   {Args: []argSpec{{Name: "user", Kind: "string", Required: true}}},
	"fb_role_create": {Args: []argSpec{{Name: "role", Kind: "string", Required: true}}},
	"fb_grant": {Args: []argSpec{
		{Name: "privilege", Kind: "string", Required: true, Enum: []string{"SELECT", "INSERT", "UPDATE", "DELETE", "EXECUTE", "ALL", "USAGE"}},
		{Name: "on", Kind: "string", Doc: "object type, e.g. TABLE or PROCEDURE"},
		{Name: "name", Kind: "string", Required: true, Doc: "object name"},
		{Name: "to", Kind: "string", Required: true, Doc: "user or role"},
	}},
	"fb_revoke": {Args: []argSpec{
		{Name: "privilege", Kind: "string", Required: true, Enum: []string{"SELECT", "INSERT", "UPDATE", "DELETE", "EXECUTE", "ALL", "USAGE"}},
		{Name: "on", Kind: "string", Doc: "object type, e.g. TABLE or PROCEDURE"},
		{Name: "name", Kind: "string", Required: true, Doc: "object name"},
		{Name: "from", Kind: "string", Doc: "user or role"},
		{Name: "to", Kind: "string", Doc: "same as from"},
	}, OneOf: [][]string{{"from", "to"}}},
	"fb_session_kill": {Args: []argSpec{
		{Name: "attachment_id", Kind: "int", Doc: "MON$ATTACHMENT_ID from fb_sessions"},
		{Name: "attachment_ids", Kind: "ints", Doc: "several attachment ids"},
	}, OneOf: [][]string{{"attachment_id", "attachment_ids"}}},
	"fb_comment_set": {Args: []argSpec{
		{Name: "on", Kind: "string", Required: true, Enum: []string{"TABLE", "COLUMN"}},
		{Name: "name", Kind: "string", Required: true, Doc: "table name"},
		{Name: "column", Kind: "string", Doc: "column name (on=COLUMN)"},
		{Name: "text", Kind: "text", Required: true, Doc: "the comment; empty removes it"},
	}},
	"fb_db_create": {Args: []argSpec{{Name: "filename", Kind: "string", Required: true, Doc: "name.fdb, created in the template's work dir"}}},
	"fb_db_drop":   {},
	"fb_shutdown_window": {Args: []argSpec{
		{Name: "mode", Kind: "string", Enum: []string{"force", "attach", "tran"}, Doc: "gfix -shut mode, default force"},
		{Name: "kick", Kind: "bool", Doc: "terminate remaining attachments"},
	}, Note: "Needs fb_window_open and a verified backup < 24 h."},
	"fb_config_set": {Args: []argSpec{
		{Name: "instance", Kind: "string", Doc: "default: instance of db"},
		{Name: "param", Kind: "string", Required: true, Doc: "firebird.conf parameter, e.g. WireCrypt"},
		{Name: "value", Kind: "text", Required: true, Doc: "new value"},
	}, Note: "Not fbmcp.yaml (fb_config_reload) or fbagent jobs (fb_agent_config_set)."},
	"fb_window_open":     {Args: []argSpec{{Name: "hours", Kind: "int", Min: 1, Max: 24, Doc: "window length, default 2"}}},
	"fb_schedule_delete": {Args: []argSpec{{Name: "id", Kind: "string", Required: true, Doc: "sch… id from fb_schedule_list"}}},
	"fb_retention_run": {Args: []argSpec{
		{Name: "keep_days", Kind: "int", Min: 0, Max: 3650, Doc: "default: retention.keep_days from fbmcp.yaml"},
		{Name: "dry_run", Kind: "bool", Doc: "default true: only list what would be deleted"},
	}},
	"fb_service_start":   {Args: []argSpec{{Name: "instance", Kind: "string", Doc: "default: instance of db"}}, Note: "OS service; with fbagent use fb_agent_start."},
	"fb_service_stop":    {Args: []argSpec{{Name: "instance", Kind: "string", Doc: "default: instance of db"}}, Note: "OS service; with fbagent use fb_agent_stop."},
	"fb_service_restart": {Args: []argSpec{{Name: "instance", Kind: "string", Doc: "default: instance of db"}}, Note: "OS service; with fbagent use fb_agent_restart."},
	"fb_agent_config_reload": {Args: []argSpec{agentArgSpec},
		Note: "fbmcp.yaml: fb_config_reload."},
	"fb_agent_pprof": {Args: []argSpec{agentArgSpec,
		{Name: "cpu_seconds", Kind: "int", Min: 0, Max: 30, Doc: "default 5; 0 = heap + goroutine only"},
	}},
	"fb_agent_run_cancel": {Args: []argSpec{agentArgSpec,
		{Name: "run_id", Kind: "string", Required: true, Doc: "run id from fb_agent_runs"},
	}},
	"fb_agent_license_set": {Args: []argSpec{agentArgSpec,
		{Name: "license", Kind: "string", Required: true, Secret: true, Doc: "the vendor JSON document, verbatim"},
	}},
	"fb_agent_restart": {Args: agentEngineSpecs, Note: "Without fbagent: fb_service_restart."},
	"fb_agent_stop":    {Args: agentEngineSpecs, Note: "Without fbagent: fb_service_stop."},
	"fb_agent_start":   {Args: agentEngineSpecs, Note: "Without fbagent: fb_service_start."},
	"fb_agent_update_apply": {Args: []argSpec{agentArgSpec, reasonArgSpec},
		Note: "Check first with fb_agent_update_status."},
	"fb_agent_product_update_apply": {Args: []argSpec{agentArgSpec, reasonArgSpec,
		{Name: "product", Kind: "string", Required: true, Enum: []string{"hqmonitor", "hqclusternode", "hqbirdrcm"}},
	}, Note: "Check first with fb_agent_product_update_status."},
	"fb_agent_cluster_pairing": {Args: []argSpec{agentArgSpec, reasonArgSpec,
		{Name: "action", Kind: "string", Enum: []string{"status", "on", "off"}, Doc: "status (default: read, no confirmation), on = open the window, off = close it"},
		{Name: "hours", Kind: "int", Min: 1, Max: 24, Doc: "with on: how long the window stays open, default 24"},
		{Name: "rcm_allow", Kind: "string", Doc: "with on: who may invite this host, comma-separated IP addresses, CIDR ranges, host names; any = any RCM; absent keeps the list"},
	}, Note: "Use it on a replica host before fb_cluster_probe / fb_cluster_link (since kit 2027.2.29 the setup opens no window unless --cluster-pairing on), or when the 24 h window is over."},
	"fb_agent_cluster_rcm": {Args: []argSpec{agentArgSpec, reasonArgSpec,
		{Name: "cluster_id", Kind: "string", Doc: "id of the new cluster: a DNS label (a-z, 0-9, -); default from the host name"},
	}, Note: "On the master host only. Check first with fb_agent_cluster. Then sudo fbmcp ctl hqbird-install on the host writes the rcm: section and the password secret."},
}

var clusterNodeArgSpec = argSpec{Name: "node", Kind: "string", Required: true, Doc: "node id from fb_cluster_status view=nodes"}

// clusterArgSpecs are the gated fb_cluster_* tools.
var clusterArgSpecs = map[string]toolArgs{
	"fb_cluster_rcm_password": {Note: "Run it once after the install: the first RCM password must change."},
	"fb_cluster_link": {Args: []argSpec{
		{Name: "hosts", Kind: "string", Required: true, Doc: "the rows to link, comma-separated: host (every agent of the host: pairing ports 7450-7459) or host:port (one agent), as fb_cluster_probe shows them"},
		{Name: "master", Kind: "string", Doc: "the host (as in hosts) that gets the master; default the first"},
		{Name: "rcm_url", Kind: "string", Doc: "URL the agents join; default the RCM address the probe shows"},
		{Name: "new_master", Kind: "bool", Doc: "one row that gets a master of its own in a cluster that has a master already (another instance pair)"},
		{Name: "replica_of", Kind: "string", Doc: "a master member id of the cluster: every row gets a replica of it (add a replica to a running pair); not with master or new_master"},
	}, Note: "Probe first: fb_cluster_probe. One free row into a cluster that has a master needs replica_of or new_master (RCM refuses lone_master)."},
	"fb_cluster_replica_folder": {Args: []argSpec{clusterNodeArgSpec,
		{Name: "root", Kind: "string", Doc: "folder; default /databases/replica/<node>"},
	}},
	"fb_cluster_enroll": {Args: []argSpec{clusterNodeArgSpec,
		{Name: "root", Kind: "string", Required: true, Doc: "folder of the master databases, e.g. /databases/master"},
		{Name: "recursive", Kind: "bool", Doc: "include subfolders"},
		{Name: "template", Kind: "string", Doc: "replication template"},
		{Name: "scansync", Kind: "bool", Doc: "scan and sync at once"},
	}},
	"fb_cluster_publication": {Args: []argSpec{clusterNodeArgSpec,
		{Name: "db_id", Kind: "string", Doc: "one database; default all databases of the node"},
	}},
	"fb_cluster_firebird_restart": {Args: []argSpec{clusterNodeArgSpec}, Note: "Needs an open window on db (fb_window_open)."},
	"fb_cluster_reinit": {Args: []argSpec{clusterNodeArgSpec,
		{Name: "db_id", Kind: "string", Doc: "database id on the master node; empty or all = every database of the node"},
		{Name: "to", Kind: "string", Doc: "replica node id; default every replica of the database"},
		{Name: "mode", Kind: "string", Enum: []string{"smooth", "standard"}, Doc: "smooth keeps the master online (default of RCM)"},
		{Name: "remove_old_replica", Kind: "bool", Doc: "delete the old replica file"},
		{Name: "allow_restart", Kind: "bool", Doc: "allow a Firebird restart on the replica"},
		{Name: "hold_on_long_transactions", Kind: "bool", Doc: "wait for long transactions instead of refusing"},
		{Name: "force", Kind: "bool", Doc: "start even when the replica looks in sync"},
	}, Note: "Needs an open window on db (fb_window_open). Then fb_cluster_verify."},
}

func init() {
	for k, v := range clusterArgSpecs {
		gatedArgSpecs[k] = v
	}
}

var agentEngineSpecs = []argSpec{agentArgSpec, reasonArgSpec,
	{Name: "coalesce", Kind: "bool", Doc: "join a running operation, default true"},
	{Name: "cluster_override", Kind: "bool", Doc: "bypass the hqclusternode refusal"},
}

// validateArgs checks args against the tool's spec and normalizes "text"
// values to strings. It returns a message that says what to fix.
func validateArgs(tool string, args map[string]any) error {
	spec, ok := gatedArgSpecs[tool]
	if !ok {
		return nil // tool without a spec: legacy path, unchecked
	}
	byName := map[string]argSpec{}
	for _, a := range spec.Args {
		byName[a.Name] = a
	}
	var problems []string
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a, ok := byName[k]
		if !ok {
			problems = append(problems, fmt.Sprintf("unknown key %q", k))
			continue
		}
		if msg := checkArg(a, args); msg != "" {
			problems = append(problems, msg)
		}
	}
	for _, a := range spec.Args {
		if _, present := args[a.Name]; a.Required && !present {
			problems = append(problems, fmt.Sprintf("%q is required", a.Name))
		}
	}
	for _, group := range spec.OneOf {
		found := false
		for _, k := range group {
			if _, present := args[k]; present {
				found = true
			}
		}
		if !found {
			problems = append(problems, "one of "+strings.Join(quoteAll(group), " or ")+" is required")
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("invalid args for %s: %s. Expected args: %s", tool, strings.Join(problems, "; "), argsSummary(spec))
}

func checkArg(a argSpec, args map[string]any) string {
	v := args[a.Name]
	switch a.Kind {
	case "bool":
		if _, ok := v.(bool); !ok {
			return fmt.Sprintf("%q must be true or false (JSON boolean, not a string)", a.Name)
		}
	case "int":
		n, ok := toInt(v)
		if !ok {
			return fmt.Sprintf("%q must be a number", a.Name)
		}
		if f, isF := v.(float64); isF && f != float64(n) {
			return fmt.Sprintf("%q must be a whole number", a.Name)
		}
		if a.Max > a.Min && (n < int64(a.Min) || n > int64(a.Max)) {
			return fmt.Sprintf("%q must be %d..%d", a.Name, a.Min, a.Max)
		}
	case "ints":
		list, ok := v.([]any)
		if !ok || len(list) == 0 {
			return fmt.Sprintf("%q must be a non-empty list of numbers", a.Name)
		}
		for _, x := range list {
			if _, ok := toInt(x); !ok {
				return fmt.Sprintf("%q must be a list of numbers", a.Name)
			}
		}
	case "text":
		switch t := v.(type) {
		case string:
		case float64:
			args[a.Name] = strconv.FormatFloat(t, 'f', -1, 64)
		case bool:
			args[a.Name] = strconv.FormatBool(t)
		default:
			return fmt.Sprintf("%q must be a string", a.Name)
		}
	case "string":
		s, ok := v.(string)
		if !ok {
			return fmt.Sprintf("%q must be a string", a.Name)
		}
		if a.Required && strings.TrimSpace(s) == "" {
			return fmt.Sprintf("%q must not be empty", a.Name)
		}
		if len(a.Enum) > 0 && s != "" {
			for _, e := range a.Enum {
				if strings.EqualFold(e, s) {
					return ""
				}
			}
			return fmt.Sprintf("%q must be one of %s", a.Name, strings.Join(a.Enum, "|"))
		}
	}
	return ""
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strconv.Quote(s)
	}
	return out
}

// argsSummary renders the args contract in one line, e.g.
// on (bool, required): true = Forced Writes ON, false = OFF.
// Compact on purpose: every description is in every client's context.
func argsSummary(spec toolArgs) string {
	if len(spec.Args) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(spec.Args))
	for _, a := range spec.Args {
		typ := a.Kind
		switch {
		case len(a.Enum) > 0:
			typ = strings.Join(a.Enum, "|")
		case a.Kind == "text":
			typ = "string"
		case a.Kind == "ints":
			typ = "int list"
		case a.Kind == "int" && a.Max > a.Min:
			typ = fmt.Sprintf("int %d..%d", a.Min, a.Max)
		}
		if a.Required {
			typ += ", required"
		}
		p := a.Name + " (" + typ + ")"
		if a.Doc != "" {
			p += ": " + a.Doc
		}
		parts = append(parts, p)
	}
	s := strings.Join(parts, "; ")
	for _, g := range spec.OneOf {
		s += "; give " + strings.Join(g, " or ")
	}
	return s
}

// gatedDescription is the MCP description of a generic gated tool: what it
// does (the impact template), its tier, and its args. The tier rules and
// mode=preview are explained once, in the server instructions.
func gatedDescription(name string, tier int, impactFmt string) string {
	what := strings.ReplaceAll(impactFmt, "%s", "<db>")
	confirm := "Tier 1 (fb_confirm)."
	switch {
	case tier >= 3:
		confirm = "Tier 3 (disabled)."
	case tier == 2:
		confirm = "Tier 2 (operator approves)."
	}
	b := strings.Builder{}
	b.WriteString(what + " " + confirm)
	if spec, ok := gatedArgSpecs[name]; ok {
		if spec.Note != "" {
			b.WriteString(" " + spec.Note)
		}
		if len(spec.Args) > 0 {
			b.WriteString(" args: " + argsSummary(spec))
		}
	}
	return b.String()
}

// redactedArgs renders args for the approver: secrets masked, long values
// cut, keys sorted. "none" when empty.
func redactedArgs(tool string, args map[string]any) string {
	if len(args) == 0 {
		return "none (defaults apply)"
	}
	secret := map[string]bool{}
	for _, a := range gatedArgSpecs[tool].Args {
		if a.Secret {
			secret[a.Name] = true
		}
	}
	shown := make(map[string]any, len(args))
	for k, v := range args {
		if strings.HasPrefix(k, "_") {
			continue
		}
		lk := strings.ToLower(k)
		if secret[k] || strings.Contains(lk, "password") || strings.Contains(lk, "secret") || strings.Contains(lk, "token") {
			shown[k] = "******"
			continue
		}
		if s, ok := v.(string); ok && len(s) > 200 {
			shown[k] = s[:200] + fmt.Sprintf("… (%d chars)", len(s))
			continue
		}
		shown[k] = v
	}
	raw, err := json.Marshal(shown)
	if err != nil {
		return fmt.Sprintf("%v", shown)
	}
	return string(raw)
}
