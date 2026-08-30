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
