// Chat replies of the gate path (UX review W1.2 / W1.3): every pending,
// preview and denied reply ends with the exact next step, so the LLM does
// not have to guess which tool or command comes next.
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/gate"
	"github.com/aleks/fbmcp/internal/policy"
	"github.com/aleks/fbmcp/internal/state"
)

// pendingReply is the reply for a new pending action: the impact statement,
// the in-band token when tier and identity allow it, and the next step.
func pendingReply(p state.PendingAction, argHash string) string {
	var b strings.Builder
	b.WriteString(gate.ImpactStatement(p))
	if p.Tier <= 1 && !p.OOBOnly {
		tok := gate.IssueToken(p.ID, argHash)
		fmt.Fprintf(&b, "In-band token (Tier 1 only): %s\n", tok)
		fmt.Fprintf(&b, "Next: show this to the user. When they agree, call fb_confirm with request_id=%q token=%q (wait=true returns the job result).\n", p.ID, tok)
		return b.String()
	}
	fmt.Fprintf(&b, "Next: ask the operator to approve request %s out-of-band: the fbmcp tray (Approve), or on the server `fbmcp ctl approve %s`. fb_confirm cannot approve it. Then call fb_job_status with request_id=%q.\n", p.ID, p.ID, p.ID)
	return b.String()
}

// previewReply is the mode=preview reply. It also works while the request
// is blocked (no window, no fresh verified backup) and then lists the steps.
func previewReply(impact string, meta policy.ToolMeta, db string, d policy.Decision, oobOnly bool) string {
	var b strings.Builder
	b.WriteString(impact)
	fmt.Fprintf(&b, "\n\nmode=preview (informational — nothing was changed; a real call still needs confirmation)\nTool: %s | Tier: %d | Database: %s\nConfirm: %s\n",
		meta.Name, meta.Tier, db, gate.ConfirmHow(meta.Tier, oobOnly))
	if steps := blockerSteps(db, d.Blockers); len(steps) > 0 {
		b.WriteString("Blocked now — do these first:\n")
		for i, s := range steps {
			fmt.Fprintf(&b, "%d. %s\n", i+1, s)
		}
	}
	return b.String()
}

// deniedReply is the reply for a denied request, with the full checklist
// when the denial is only about missing Tier-2 requirements.
func deniedReply(tool, db string, d policy.Decision) string {
	var b strings.Builder
	b.WriteString("DENIED: " + d.Reason + "\n")
	if steps := blockerSteps(db, d.Blockers); len(steps) > 0 {
		b.WriteString("Do these first (all of them):\n")
		for i, s := range steps {
			fmt.Fprintf(&b, "%d. %s\n", i+1, s)
		}
		fmt.Fprintf(&b, "Next: do the steps above, then call %s again (mode=preview works now).\n", tool)
		return b.String()
	}
	if hint := denyHint(d.Reason); hint != "" {
		b.WriteString("Next: " + hint + "\n")
	}
	return b.String()
}

func blockerSteps(db string, blockers []policy.Blocker) []string {
	var steps []string
	backupStep := false
	for _, bl := range blockers {
		switch bl.Name {
		case policy.BlockerWindow:
			steps = append(steps, fmt.Sprintf("Open a maintenance window: fb_window_open db=%q args={\"hours\": 2} (Tier 1, confirm with fb_confirm).", db))
		case "verified_backup_exists", "backup_freshness":
			if !backupStep {
				backupStep = true
				steps = append(steps, fmt.Sprintf("Make a verified backup younger than 24 h: fb_backup_start db=%q, then fb_restore_test db=%q (a good test-restore marks the backup verified).", db, db))
			}
		default:
			steps = append(steps, bl.Why)
		}
	}
	return steps
}

// denyHint maps the policy deny reasons that have a fixed next step.
func denyHint(reason string) string {
	switch {
	case strings.Contains(reason, "exceeds identity ceiling"):
		return "this API key does not allow this tier. Ask the fbmcp operator for a key with a higher max tier (fbmcp ctl apikey)."
	case strings.Contains(reason, "has no permission for database"):
		return "this identity is limited to other databases (identities[].databases in fbmcp.yaml); call fb_db_list."
	case strings.Contains(reason, "Tier 3"):
		return "Tier 3 tools are disabled; do this task outside fbmcp."
	case strings.Contains(reason, "unknown database id"):
		return "call fb_db_list for the registered database ids."
	case strings.Contains(reason, "invalid args"):
		return "fix the args as listed above and call the tool again."
	case strings.Contains(reason, "does not meet MinFB"):
		return "this tool needs a newer Firebird version on this database."
	}
	return ""
}

// finishGated is the shared tail of the gated request paths: deny (with a
// checklist), preview, or create the pending action. impact is built only
// when it is shown. Returns (text, isError).
func (gt *gatedTools) finishGated(id policy.Identity, meta policy.ToolMeta, d policy.Decision, impact func() string, dbID string, args map[string]any, mode string) (string, bool) {
	preview := strings.EqualFold(mode, "preview")
	if d.Outcome == "deny" && !(preview && d.OnlyBlocked()) {
		gt.aud.Log(audit.Entry{Identity: id.Name, Database: dbID, Tool: meta.Name, Tier: meta.Tier, Decision: "denied", Detail: map[string]interface{}{"reason": d.Reason}})
		return deniedReply(meta.Name, dbID, d), true
	}
	text := impact()
	if preview {
		return previewReply(text, meta, dbID, d, id.OOBOnly), false
	}
	if len(d.FailedPreconditions) > 0 {
		gt.aud.Log(audit.Entry{Identity: id.Name, Database: dbID, Tool: meta.Name, Tier: meta.Tier, Decision: "denied", Detail: map[string]interface{}{"reason": d.Reason}})
		return deniedReply(meta.Name, dbID, d), true
	}
	argsJSON, _ := json.Marshal(args)
	argHash := hashOf(dbID + string(argsJSON))
	p, err := gt.g.Request(id, dbID, meta, text, argHash, d.FailedPreconditions)
	if err != nil {
		return "gate error: " + err.Error(), true
	}
	gt.storeArgs(p.ID, args)
	return pendingReply(p, argHash), false
}

// confirmedReply is the fb_confirm reply. The "job <id>" phrase is parsed by
// the smoke/wtest harnesses.
func confirmedReply(jobID string) string {
	return fmt.Sprintf("confirmed; job %s submitted.\nNext: call fb_job_status with job_id=%q until it is done (or use fb_confirm wait=true next time).", jobID, jobID)
}

// jobForRequest finds the job that an approved request started. When there
// is no job yet, msg says why and what to do.
func jobForRequest(st *state.Store, requestID string) (jobID, msg string) {
	var best state.Job
	for _, j := range st.Jobs() {
		if j.RequestID == requestID && (best.ID == "" || j.CreatedAt.After(best.CreatedAt)) {
			best = j
		}
	}
	if best.ID != "" {
		return best.ID, ""
	}
	for _, p := range st.Pending() {
		if p.ID == requestID {
			return "", fmt.Sprintf("request %s (%s on %s) is still pending; it expires %s.\nNext: wait for the approval (%s), then call fb_job_status with request_id=%q again.",
				p.ID, p.Tool, p.Database, gate.ExpiryText(p.Expires), gate.ConfirmHow(p.Tier, p.OOBOnly), requestID)
		}
	}
	return "", fmt.Sprintf("no job and no pending action for request %s: it was denied, cancelled, or expired, or the id is wrong.\nNext: request the action again if it is still needed.", requestID)
}

// storeArgs keeps a request's args for dispatch. A nil map is stored as an
// empty one: the key's presence is what dispatch checks.
func (gt *gatedTools) storeArgs(id string, args map[string]any) {
	if args == nil {
		args = map[string]any{}
	}
	gt.mu.Lock()
	gt.args[id] = args
	gt.mu.Unlock()
}
