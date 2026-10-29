// Package agentcfg builds fbagent configuration candidates (phase 11 D5b,
// ADR-037). fbmcp never edits agent_config.json in place: it writes a whole
// document as agent_config_pending_<UTC>.json beside the live file and asks
// the agent to promote it (POST /v1/config/reload) — the agent's own
// supported path, the same one HQ Monitor's editor uses. The agent validates,
// backs up, installs, activates and rolls back; fbmcp only composes.
//
// The patch is deliberately narrow: one job family (or one job / scheduler
// task in it), existing leaf settings only, same JSON type, no credentials,
// no command/binary fields. Everything else stays operator-only.
package agentcfg

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Constants shared with the agent (fbagent internal/config/candidate.go).
const (
	CandidatePrefix = "agent_config_pending"
	StampLayout     = "20060102_150405"
	// SettleTime: the agent skips a candidate younger than 2s (a half-written
	// copy). fbmcp waits this out, plus margin, before asking for the reload.
	SettleTime = 2500 * time.Millisecond
	// MaxBytes mirrors HQ Monitor's editor cap (the agent itself allows 8 MiB).
	MaxBytes = 4 << 20
)

// Families that may be patched. sql_script is excluded on purpose: its jobs
// name scripts to execute, which is code, not a setting.
var Families = map[string]bool{
	"vbackup": true, "nbackup": true, "restore": true, "sweep": true,
	"grp_sweep": true, "gfix_check": true, "index_recalc": true, "scheduler": true,
}

// taskKeys is the scheduler.tasks allowlist (command/args are code).
var taskKeys = map[string]bool{"enabled": true, "schedule": true, "timeout_seconds": true}

// deniedKey reports keys a patch must never touch: credentials (the agent's
// own IsSecretKey rule plus a wider net), identity, and anything that names a
// program to run.
func deniedKey(k string) string {
	lk := strings.ToLower(k)
	switch {
	case strings.Contains(lk, "password"), strings.Contains(lk, "token"), strings.Contains(lk, "secret"),
		strings.Contains(lk, "credential"), strings.HasSuffix(lk, "_encrypted"), strings.Contains(lk, "ntlmuser"):
		return "credentials are operator-only (HQ Monitor / agent CLI)"
	case lk == "id" || lk == "name":
		return "job identity cannot be changed"
	case lk == "command" || lk == "args" || strings.HasSuffix(lk, "binary") || lk == "gbakpath" || strings.HasSuffix(lk, "exe"):
		return "fields that name a program to run are operator-only"
	case lk == "jobs" || lk == "tasks":
		return "job lists cannot be replaced; target one job with job_id (or task for scheduler)"
	}
	return ""
}

// Fingerprint is the sha256 of the live file, used as the optimistic-lock
// token between the gated request and the approved execution.
func Fingerprint(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Result is a composed candidate.
type Result struct {
	Doc    []byte   // whole document, agent encoding (2-space indent, no BOM)
	Diff   []string // "path: old -> new", sorted
	Target string   // e.g. vbackup.individual.jobs[id=db1]
}

// Apply patches the live agent_config.json. family is required; jobID
// selects one element (by "id") of any "jobs" array inside the family; task
// selects one scheduler.tasks entry by name (family must be "scheduler").
func Apply(live []byte, family, jobID, task string, patch map[string]any) (*Result, error) {
	if bytes.HasPrefix(live, []byte{0xEF, 0xBB, 0xBF}) {
		live = live[3:]
	}
	family = strings.ToLower(strings.TrimSpace(family))
	if !Families[family] {
		return nil, fmt.Errorf("family %q is not patchable (allowed: %s)", family, strings.Join(sortedKeys(Families), ", "))
	}
	if len(patch) == 0 {
		return nil, fmt.Errorf("patch is empty")
	}
	dec := json.NewDecoder(bytes.NewReader(live))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("live agent_config.json is not a JSON object: %w", err)
	}
	for _, s := range []string{"agent", "firebird", "scheduler"} {
		if _, ok := doc[s].(map[string]any); !ok {
			return nil, fmt.Errorf("live agent_config.json lacks the %q section — refusing to compose a candidate from it", s)
		}
	}
	fam, ok := doc[family].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("family %q not present in the live config", family)
	}
	target, path := fam, family
	switch {
	case family == "scheduler":
		if task == "" || jobID != "" {
			return nil, fmt.Errorf("scheduler: pass task (a scheduler.tasks name); job_id does not apply")
		}
		tasks, _ := fam["tasks"].([]any)
		target = nil
		for _, t := range tasks {
			if m, ok := t.(map[string]any); ok && m["name"] == task {
				target = m
			}
		}
		if target == nil {
			return nil, fmt.Errorf("scheduler task %q not found", task)
		}
		for k := range patch {
			if !taskKeys[k] {
				return nil, fmt.Errorf("scheduler task key %q not allowed (allowed: enabled, schedule, timeout_seconds)", k)
			}
		}
		path = fmt.Sprintf("scheduler.tasks[name=%s]", task)
	case task != "":
		return nil, fmt.Errorf("task applies only to family scheduler")
	case jobID != "":
		var hits []string
		target = nil
		findJobs(fam, family, jobID, &target, &hits)
		switch len(hits) {
		case 0:
			return nil, fmt.Errorf("job_id %q not found in %s", jobID, family)
		case 1:
			path = hits[0]
		default:
			return nil, fmt.Errorf("job_id %q is ambiguous in %s: %s", jobID, family, strings.Join(hits, ", "))
		}
	}
	var diff []string
	if err := merge(target, patch, path, family != "scheduler", &diff); err != nil {
		return nil, err
	}
	if len(diff) == 0 {
		return nil, fmt.Errorf("patch changes nothing (values already set)")
	}
	sort.Strings(diff)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if buf.Len() > MaxBytes {
		return nil, fmt.Errorf("candidate is %d bytes, over the %d byte limit", buf.Len(), MaxBytes)
	}
	return &Result{Doc: buf.Bytes(), Diff: diff, Target: path}, nil
}

func findJobs(v any, path, id string, target *map[string]any, hits *[]string) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	for _, k := range sortedKeys(m) {
		child := m[k]
		if k == "jobs" {
			if arr, ok := child.([]any); ok {
				for _, e := range arr {
					if jm, ok := e.(map[string]any); ok && jm["id"] == id {
						*target = jm
						*hits = append(*hits, fmt.Sprintf("%s.jobs[id=%s]", path, id))
					}
				}
			}
			continue
		}
		findJobs(child, path+"."+k, id, target, hits)
	}
}

// merge applies patch onto dst: existing keys only, same JSON kind, objects
// recurse, scalars and arrays replace.
func merge(dst map[string]any, patch map[string]any, path string, guardCommand bool, diff *[]string) error {
	for _, k := range sortedKeys(patch) {
		pv := patch[k]
		p := path + "." + k
		if why := deniedKey(k); why != "" {
			return fmt.Errorf("%s: %s", p, why)
		}
		// Turning on a block that runs a configured command (vbackup
		// postProcessing) is the same as running that command.
		if guardCommand && strings.EqualFold(k, "enabled") {
			if cmd, _ := dst["command"].(string); strings.TrimSpace(cmd) != "" {
				return fmt.Errorf("%s: this block runs a command; enabling it is operator-only", p)
			}
		}
		cur, exists := dst[k]
		if !exists {
			return fmt.Errorf("%s: unknown setting (only existing settings can be changed; check the name with fb_agent_backup_config)", p)
		}
		if pv == nil {
			return fmt.Errorf("%s: null (delete) is not allowed", p)
		}
		if s, ok := pv.(string); ok && s == "***" {
			return fmt.Errorf("%s: the redaction placeholder *** cannot be written", p)
		}
		switch c := cur.(type) {
		case map[string]any:
			pm, ok := pv.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: expected an object", p)
			}
			if err := merge(c, pm, p, guardCommand, diff); err != nil {
				return err
			}
		case json.Number:
			n, err := toNumber(pv, !strings.ContainsAny(c.String(), ".eE"))
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			if n.String() != c.String() {
				*diff = append(*diff, fmt.Sprintf("%s: %s -> %s", p, c, n))
				dst[k] = n
			}
		case string:
			s, ok := pv.(string)
			if !ok {
				return fmt.Errorf("%s: expected a string", p)
			}
			if s != c {
				*diff = append(*diff, fmt.Sprintf("%s: %q -> %q", p, c, s))
				dst[k] = s
			}
		case bool:
			b, ok := pv.(bool)
			if !ok {
				return fmt.Errorf("%s: expected true/false", p)
			}
			if b != c {
				*diff = append(*diff, fmt.Sprintf("%s: %v -> %v", p, c, b))
				dst[k] = b
			}
		case []any:
			pa, ok := pv.([]any)
			if !ok {
				return fmt.Errorf("%s: expected an array", p)
			}
			for _, e := range pa {
				if _, isObj := e.(map[string]any); isObj {
					return fmt.Errorf("%s: arrays of objects cannot be replaced", p)
				}
			}
			if !reflect.DeepEqual(normArr(pa), normArr(c)) {
				o, _ := json.Marshal(c)
				n, _ := json.Marshal(pa)
				*diff = append(*diff, fmt.Sprintf("%s: %s -> %s", p, o, n))
				dst[k] = pa
			}
		case nil:
			return fmt.Errorf("%s: current value is null — type unknown, set it in HQ Monitor", p)
		default:
			return fmt.Errorf("%s: unsupported current type %T", p, cur)
		}
	}
	return nil
}

func normArr(a []any) string { b, _ := json.Marshal(a); return string(b) }

func toNumber(v any, integer bool) (json.Number, error) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case json.Number:
		var err error
		if f, err = n.Float64(); err != nil {
			return "", fmt.Errorf("expected a number")
		}
	default:
		return "", fmt.Errorf("expected a number")
	}
	if integer {
		if f != math.Trunc(f) {
			return "", fmt.Errorf("expected an integer")
		}
		return json.Number(strconv.FormatInt(int64(f), 10)), nil
	}
	return json.Number(strconv.FormatFloat(f, 'f', -1, 64)), nil
}

// Pending lists candidates already waiting beside configPath (not marked
// superseded). A waiting one would be overtaken by ours — refuse instead of
// silently discarding someone's edit.
func Pending(configPath string) ([]string, error) {
	m, err := filepath.Glob(filepath.Join(filepath.Dir(configPath), CandidatePrefix+"*.json"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range m {
		if _, err := os.Stat(p + ".superseded"); err == nil {
			continue
		}
		out = append(out, filepath.Base(p))
	}
	return out, nil
}

// WriteCandidate writes doc as agent_config_pending_<UTC>[-n].json beside
// configPath, atomically (temp file that does not match the agent's glob,
// then rename). Returns the candidate file name.
func WriteCandidate(configPath string, doc []byte, now time.Time) (string, error) {
	if bytes.HasPrefix(doc, []byte{0xEF, 0xBB, 0xBF}) {
		return "", fmt.Errorf("candidate starts with a BOM")
	}
	dir := filepath.Dir(configPath)
	base := filepath.Join(dir, CandidatePrefix+"_"+now.UTC().Format(StampLayout))
	dst := ""
	for i := 1; i <= 50; i++ {
		p := base + ".json"
		if i > 1 {
			p = fmt.Sprintf("%s-%d.json", base, i)
		}
		if _, err := os.Stat(p); os.IsNotExist(err) {
			dst = p
			break
		}
	}
	if dst == "" {
		return "", fmt.Errorf("no free candidate name in %s", dir)
	}
	tmp := filepath.Join(dir, ".fbmcp_candidate_"+strconv.FormatInt(now.UnixNano(), 10)+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("create candidate in %s: %w (the fbmcp account needs create rights on the agent directory)", dir, err)
	}
	if _, err := f.Write(doc); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return filepath.Base(dst), nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
