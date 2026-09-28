// Package errdoc classifies error messages fbmcp tools actually produce
// into stable codes with hints and remediation (phase8_plan D3.1 /
// improvement-plan D.2). Consumers: the errText dispatch wrapper that puts
// {code, message, hint, remediation} into structuredContent and sets
// IsError, so LLM clients can react programmatically instead of parsing
// ad-hoc text prefixes.
package errdoc

import "strings"

// Doc is the classification for one error signal.
type Doc struct {
	Code        string
	Hint        string
	Remediation string
}

// signals are matched as substrings of the error text, in order (first hit
// wins). Seed set: the failure modes observed live on the dev host plus the
// documented deny paths.
var signals = []struct {
	match string
	doc   Doc
}{
	// fbagent Local API bridge (phase9_plan.md A1). These MUST stay at the
	// top: the agent's free-form error messages can contain substrings that
	// generic signals below would otherwise misclassify first (e.g. the job
	// name find_deadlocks tripping the deadlock signal).
	{
		match: "fbagent transport",
		doc: Doc{
			Code:        "agent-unreachable",
			Hint:        "The fbagent Local API endpoint could not be reached (not running, wrong base_url, or loopback blocked).",
			Remediation: "Check the agent service is running with local_api.enabled and that agents[].base_url matches its listen address.",
		},
	},
	{
		match: "fbagent auth",
		doc: Doc{
			Code:        "agent-auth",
			Hint:        "The agent rejected the Bearer token (401; the API denies every request without a valid token).",
			Remediation: "Check agents[].token_env: the env var (or keyring entry fbmcp/<name>) must hold the agent's local_api.token.",
		},
	},
	{
		match: "fbagent not-found",
		doc: Doc{
			Code:        "agent-not-found",
			Hint:        "The agent has no such route, job, instance, operation, or artifact (or a pruned operation id was polled).",
			Remediation: "Check the job/operation/artifact name against the fbagent API reference for the deployed agent version.",
		},
	},
	{
		match: "fbagent bad-request",
		doc: Doc{
			Code:        "agent-remote-denied",
			Hint:        "The agent refused the request parameters (400/405 denied).",
			Remediation: "Fix the arguments (unknown job_id, missing sql_script scriptPath/scriptBody, invalid glob or path).",
		},
	},
	{
		match: "fbagent unavailable",
		doc: Doc{
			Code:        "agent-unavailable",
			Hint:        "The requested feature is not wired on the agent (missing data_path, config, or install path; or a capture already in progress).",
			Remediation: "Check the agent's agent_config.json covers the requested feature; retry later for in-progress captures.",
		},
	},
	{
		match: "fbagent service-error",
		doc: Doc{
			Code:        "agent-remote-error",
			Hint:        "The agent's underlying operation failed (read error, SCM failure, validation surfaced by reload).",
			Remediation: "Inspect the agent's own logs for the failing operation; the message carries the agent's error text.",
		},
	},
	{
		match: "fbagent op-timeout",
		doc: Doc{
			Code:        "agent-timeout",
			Hint:        "The agent operation did not reach a terminal state within the bridge timeout (default agents[].timeout_seconds=130), or the agent reported readiness timeout.",
			Remediation: "Poll later with fb_agent_operation using the returned operation_id; raise agents[].timeout_seconds above the agent's restart_timeout_seconds if this recurs.",
		},
	},
	{
		match: "fbagent payload-too-large",
		doc: Doc{
			Code:        "agent-payload-too-large",
			Hint:        "The artifact exceeded the agent's decoded-response cap (default 8 MiB).",
			Remediation: "Retry with tail=N (db-scoped */latest artifacts) or a narrower glob/date filter.",
		},
	},
	{
		match: "MySQLEngine",
		doc: Doc{
			Code:        "engine-plugin-scan",
			Hint:        "On HQBird builds this plugin error usually masks the real condition: the database FILE does not exist (the engine-provider scan trips over an unloadable plugin before reporting 'unavailable database').",
			Remediation: "Check the database file path exists before blaming plugins (verified live on the dev host).",
		},
	},
	{
		match: "in use by concurrent transaction",
		doc: Doc{
			Code:        "snapshot-conflict",
			Hint:        "Another attachment holds an open snapshot transaction on the target object.",
			Remediation: "Retry after the competing transaction ends; exclusive operations (non-CONCURRENTLY REFRESH) drain fbmcp's pools automatically — look for external attachments via the sessions tool.",
		},
	},
	{
		match: "deadlock",
		doc: Doc{
			Code:        "deadlock",
			Hint:        "Two transactions waited on each other; the engine rolled one back.",
			Remediation: "Retry the statement; if persistent, inspect conflicting transactions via the transactions tool.",
		},
	},
	{
		match: "unavailable database",
		doc: Doc{
			Code:        "database-offline",
			Hint:        "The Firebird instance is unreachable or the database file is missing.",
			Remediation: "Check instance service status and the database file; use the health and service-status tools.",
		},
	},
	{
		match: "verified_backup_exists",
		doc: Doc{
			Code:        "precondition-backup",
			Hint:        "Tier-2 operations require a catalog backup marked verified and fresh (< 24 h).",
			Remediation: "Run a backup, then a test-restore (that marks it verified), and open a maintenance window.",
		},
	},
	{
		match: "no open maintenance window",
		doc: Doc{
			Code:        "precondition-window",
			Hint:        "Tier-2 tools require an open maintenance window.",
			Remediation: "Open a window first (window tool, Tier 1), then re-request.",
		},
	},
	{
		match: "another fbmcp instance is active",
		doc: Doc{
			Code:        "single-kernel-lock",
			Hint:        "A kernel already holds this state dir (single-instance lock, D8).",
			Remediation: "Extra piped stdio clients attach automatically; a second kernel must use a different state dir.",
		},
	},
	{
		match: "channel policy",
		doc: Doc{
			Code:        "channel-policy",
			Hint:        "This tier cannot be confirmed in-band — by design.",
			Remediation: "Approve out-of-band: fbmcpctl approve <state_dir> <request_id> or the tray popup.",
		},
	},
	{
		match: "connection refused",
		doc: Doc{
			Code:        "instance-unreachable",
			Hint:        "The Firebird service is not listening on the configured address.",
			Remediation: "Check the instance service status; start it if stopped (service tools, Tier 2).",
		},
	},
	{
		match: "fb_write is for mutations",
		doc: Doc{
			Code:        "fbwrite-read-denied",
			Hint:        "fb_write executes mutations only; reads never need its confirmation gate.",
			Remediation: "Call fb_query (Tier 0): one SELECT or EXECUTE PROCEDURE on a read-only transaction.",
		},
	},
	{
		match: "fb_query accepts",
		doc: Doc{
			Code:        "fbquery-rejected",
			Hint:        "fb_query runs single read statements only (one SELECT/WITH-SELECT or EXECUTE PROCEDURE).",
			Remediation: "Use fb_write for DML/DDL/DCL scripts and EXECUTE BLOCK (confirmation required).",
		},
	},
	{
		match: "attempted update during read-only transaction",
		doc: Doc{
			Code:        "ro-write-refused",
			Hint:        "The engine refused a write on fb_query's read-only transaction (typically an EXECUTE PROCEDURE that mutates).",
			Remediation: "fb_query routes such calls into fb_write's gated flow — confirm the pending action (Tier 1 in-band token) or call fb_write directly.",
		},
	},
	// SQL compile errors (phase10 fb_query_check). The object-missing
	// matches MUST stay above the generic "Dynamic SQL Error": a prepare
	// failure like "Dynamic SQL Error / Table unknown EMPLOYEE" contains
	// both substrings and first hit wins.
	{
		match: "Table unknown",
		doc: Doc{
			Code:        "sql-object-missing",
			Hint:        "The statement references a table that does not exist in this database (or the read-only user cannot see it).",
			Remediation: "Check the name and case via the schema/describe tools; drafts come from the query-builder playbook.",
		},
	},
	{
		match: "Column unknown",
		doc: Doc{
			Code:        "sql-object-missing",
			Hint:        "The statement references a column that does not exist on the queried objects.",
			Remediation: "Describe the table and fix the column name (never invent one).",
		},
	},
	{
		match: "Procedure unknown",
		doc: Doc{
			Code:        "sql-object-missing",
			Hint:        "The statement calls a stored procedure that does not exist in this database.",
			Remediation: "Check the procedure name via the schema tools; selectable procedures run through SELECT.",
		},
	},
	{
		match: "Function unknown",
		doc: Doc{
			Code:        "sql-object-missing",
			Hint:        "The statement calls a function that does not exist in this database.",
			Remediation: "Check the function name via the schema tools.",
		},
	},
	{
		match: "Dynamic SQL Error",
		doc: Doc{
			Code:        "sql-compile",
			Hint:        "The engine refused to compile the statement (syntax, token, or datatype error at prepare time).",
			Remediation: "Fix the failing statement; validate drafts with fb_query_check — it compiles without executing.",
		},
	},
	// Grid resource reads (phase10 uigrid). The codes also lead the raw
	// error text of a resources/read, so clients see the stable code even
	// though resource reads carry no structuredContent.
	{
		match: "result-expired",
		doc: Doc{
			Code:        "result-expired",
			Hint:        "The grid result id is unknown or its TTL passed (in-memory registry only; a kernel restart clears it).",
			Remediation: "Re-run the query; the fresh result carries a new ui resource reference.",
		},
	},
	{
		match: "result-forbidden",
		doc: Doc{
			Code:        "result-forbidden",
			Hint:        "The grid result belongs to another MCP identity (registry reads are identity-scoped).",
			Remediation: "Re-run the query under the identity that produced it.",
		},
	},
}

// Lookup classifies an error message; ok=false leaves the generic code.
func Lookup(msg string) (Doc, bool) {
	for _, s := range signals {
		if strings.Contains(msg, s.match) {
			return s.doc, true
		}
	}
	return Doc{}, false
}
