// Package ctl is the operator CLI (P5.6): approve, status, setup, doctor,
// hqbird-install. It runs as "fbmcp ctl ..." and as the fbmcpctl wrapper.
package ctl

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/gate"
	"github.com/aleks/fbmcp/internal/notify"
	"github.com/aleks/fbmcp/internal/posture"
	"github.com/aleks/fbmcp/internal/secrets"
	"github.com/aleks/fbmcp/internal/state"
)

// Main runs one operator command (args without the program name) and
// returns the exit code. fbmcpctl and "fbmcp ctl" both land here (ADR-038).
func Main(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: fbmcp ctl <approve|status|setup|doctor|verify|repair|secret|apikey|tls|gate|ping|hqbird-install> ...")
		return 2
	}
	switch args[0] {
	case "approve":
		return cmdApprove(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "setup":
		return cmdSetup(args[1:])
	case "doctor":
		return cmdDoctor(args[1:])
	case "verify":
		return cmdVerify(args[1:])
	case "repair":
		return cmdRepair(args[1:])
	case "secret":
		return cmdSecret(args[1:])
	case "apikey":
		return cmdAPIKey(args[1:])
	case "tls":
		return cmdTLS(args[1:])
	case "gate":
		return cmdGate(args[1:])
	case "ping":
		return cmdPing(args[1:])
	case "hqbird-install":
		return cmdHqbirdInstall(args[1:])
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", args[0])
		return 2
	}
}

// cmdRepair rebuilds enriched-era schedule grants from audit.jsonl into a
// fresh state store (phase8_plan D1.2b). Only grants approved AFTER the
// audit-detail enrichment carry enough information; pre-enrichment entries
// are counted and skipped. Deletes are not replayed; pending actions are
// not rebuilt by design (15-min TTL). A corrupt state.json is moved aside
// as state.json.pre-repair-<ts> first — evidence is never destroyed.
func cmdRepair(args []string) int {
	cfgPath := "fbmcp.yaml"
	for _, a := range args {
		if a != "" && a[0] != '-' && a != "--from-audit" {
			cfgPath = a
		}
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	auditPath := filepath.Join(cfg.State.Dir, "audit.jsonl")
	b, err := os.ReadFile(auditPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "audit log unreadable:", err)
		return 1
	}

	// A loadable, non-empty state.json means repair is the wrong tool.
	if st, serr := state.Open(cfg.State.Dir); serr == nil {
		if len(st.Schedules()) > 0 || len(st.Jobs()) > 0 || len(st.Pending()) > 0 {
			fmt.Fprintln(os.Stderr, "state.json loads and is non-empty - repair is only for a corrupt or absent state")
			return 1
		}
	} else {
		// corrupt: move it aside (the .corrupt-* evidence copy already exists)
		old := filepath.Join(cfg.State.Dir, "state.json")
		aside := fmt.Sprintf("%s.pre-repair-%d", old, time.Now().Unix())
		if err := os.Rename(old, aside); err != nil {
			fmt.Fprintln(os.Stderr, "cannot move corrupt state aside:", err)
			return 1
		}
		fmt.Println("corrupt state.json moved aside:", aside)
	}
	st, err := state.Open(cfg.State.Dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "state:", err)
		return 1
	}

	rebuilt, skipped := 0, 0
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var e audit.Entry
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			fmt.Fprintln(os.Stderr, "audit line unparseable - chain integrity is prerequisite (run verify):", err)
			return 1
		}
		if e.Tool != "fb_schedule_create" || e.Decision != "approved" {
			continue
		}
		d := e.Detail
		if d == nil {
			continue
		}
		cron, _ := d["cron"].(string)
		argHash, _ := d["arg_hash"].(string)
		if cron == "" || argHash == "" {
			skipped++ // pre-enrichment entry: not reconstructable
			continue
		}
		str := func(k string) string { v, _ := d[k].(string); return v }
		missed := str("missed_run")
		if missed == "" {
			missed = "skip"
		}
		win, _ := d["window_required"].(bool)
		sc := state.Schedule{
			ID: str("schedule_id"), Database: e.Database, Target: str("target"), Kind: str("kind"),
			ArgsJSON: str("args"), ArgHash: argHash, MaxTier: e.Tier, Confirmer: e.Identity,
			Channel: e.Channel, CreatingRequest: str("creating_request"),
			Cron: cron, Timezone: str("timezone"), WindowRequired: win, MissedRun: missed,
			Overlap: "skip", Enabled: true, CreatedAt: e.Time,
		}
		if sc.ID == "" || sc.Target == "" {
			skipped++
			continue
		}
		if err := st.PutSchedule(sc); err != nil {
			fmt.Fprintln(os.Stderr, "rebuild failed:", err)
			return 1
		}
		fmt.Printf("rebuilt grant %s db=%s target=%s cron=%q\n", sc.ID, sc.Database, sc.Target, sc.Cron)
		rebuilt++
	}
	fmt.Printf("repair complete: %d grants rebuilt, %d pre-enrichment entries skipped\n", rebuilt, skipped)
	fmt.Println("not rebuilt: pending actions (TTL'd by design); schedule deletes (re-apply manually if needed)")
	fmt.Println("next: fbmcpctl verify", cfgPath, "&& fbmcpctl doctor", cfgPath)
	return 0
}

// cmdSecret manages keyring fallback entries (D4.3 / E.4): the value is
// read from stdin, never argv (C10). set <ENV_NAME>, get <ENV_NAME>,
// drop <ENV_NAME>. Environment variables still win at lookup time.
// --dir DIR uses the file source instead (phase 12 B0.9 / B2): one file
// per name, mode 0600, owned like DIR when run as root.
func cmdSecret(args []string) int {
	var dir string
	var rest []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--dir" && i+1 < len(args):
			dir = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--dir="):
			dir = strings.TrimPrefix(args[i], "--dir=")
		default:
			rest = append(rest, args[i])
		}
	}
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "usage: fbmcpctl secret set|get|drop [--dir DIR] <ENV_NAME>  (set reads the value from stdin)")
		return 2
	}
	op, name := rest[0], rest[1]
	if dir != "" {
		return secretFile(op, dir, name)
	}
	switch op {
	case "set":
		v, code := readSecretStdin()
		if code != 0 {
			return code
		}
		if err := secrets.Set(name, v); err != nil {
			fmt.Fprintln(os.Stderr, "keyring:", err)
			return 1
		}
		fmt.Printf("stored fbmcp/%s in the OS keyring (env %s still wins if set)\n", name, name)
		return 0
	case "get":
		if os.Getenv(name) != "" {
			fmt.Printf("env %s is set - the keyring entry is shadowed\n", name)
			return 0
		}
		if _, err := secrets.Get(name); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("keyring entry fbmcp/%s present\n", name)
		return 0
	case "drop":
		if err := secrets.Drop(name); err != nil {
			fmt.Fprintln(os.Stderr, "keyring:", err)
			return 1
		}
		fmt.Printf("dropped fbmcp/%s\n", name)
		return 0
	}
	fmt.Fprintln(os.Stderr, "unknown secret op:", op)
	return 2
}

func readSecretStdin() (string, int) {
	val, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return "", 1
	}
	v := strings.TrimSpace(string(val))
	if v == "" {
		fmt.Fprintln(os.Stderr, "empty value on stdin")
		return "", 1
	}
	return v, 0
}

// secretFile is cmdSecret for the file source. get never prints the value.
func secretFile(op, dir, name string) int {
	switch op {
	case "set":
		v, code := readSecretStdin()
		if code != 0 {
			return code
		}
		if err := secrets.WriteFile(dir, name, v); err != nil {
			fmt.Fprintln(os.Stderr, "secret file:", err)
			return 1
		}
		fmt.Printf("stored %s (env %s still wins if set); a running kernel picks it up in a few seconds\n", filepath.Join(dir, name), name)
		return 0
	case "get":
		if _, err := secrets.ReadFile(dir, name); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("file %s present and readable\n", filepath.Join(dir, name))
		if os.Getenv(name) != "" {
			fmt.Printf("note: env %s is set in this shell and shadows the file\n", name)
		}
		return 0
	case "drop":
		if err := secrets.DropFile(dir, name); err != nil {
			fmt.Fprintln(os.Stderr, "secret file:", err)
			return 1
		}
		fmt.Printf("removed %s\n", filepath.Join(dir, name))
		return 0
	}
	fmt.Fprintln(os.Stderr, "unknown secret op:", op)
	return 2
}

// cmdVerify runs the standalone audit-chain verification (runbook §7.2):
// rehashes every entry and checks the chain + head sidecar.
func cmdVerify(args []string) int {
	cfgPath := "fbmcp.yaml"
	if len(args) > 0 {
		cfgPath = args[0]
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// Verify returns the failing line (0 on success); count entries for humans.
	path := filepath.Join(cfg.State.Dir, "audit.jsonl")
	if _, err := audit.Verify(path); err != nil {
		fmt.Fprintln(os.Stderr, "audit chain BROKEN:", err)
		return 1
	}
	n := 0
	if b, err := os.ReadFile(path); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if l != "" {
				n++
			}
		}
	}
	fmt.Printf("audit chain OK: %d entries (%s)\n", n, path)
	return 0
}

func cmdApprove(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: fbmcpctl approve <state_dir> [request_id]")
		return 2
	}
	dir := args[0]
	st, err := state.Open(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "state:", err)
		return 1
	}
	pending := st.Pending()
	if len(args) == 1 {
		if len(pending) == 0 {
			fmt.Println("no pending actions")
			return 0
		}
		for _, p := range pending {
			fmt.Println(gate.ImpactStatement(p))
			fmt.Println("---")
		}
		fmt.Println("run: fbmcpctl approve <state_dir> <request_id> to approve")
		return 0
	}
	id := args[1]
	found := false
	for _, p := range pending {
		if p.ID == id {
			found = true
			fmt.Print(gate.ImpactStatement(p))
		}
	}
	if !found {
		// lab 2026-11-04: an expired id used to print "no pending actions"
		// with exit 0, so the operator took it for an approval.
		fmt.Printf("no such pending action: %s (a request expires after %s; an approved, denied or expired one leaves the list)\n", id, gate.DefaultTTL)
		return 1
	}
	approvals := filepath.Join(dir, "approvals")
	os.MkdirAll(approvals, 0o755)
	if err := os.WriteFile(filepath.Join(approvals, id), []byte("approved-by-operator\n"), 0o640); err != nil {
		fmt.Fprintln(os.Stderr, "write marker:", err)
		return 1
	}
	fmt.Println("approval recorded — the server will confirm and execute the action")
	return 0
}

func cmdStatus(args []string) int {
	cfgPath := "fbmcp.yaml"
	if len(args) > 0 {
		cfgPath = args[0]
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	st, err := state.Open(cfg.State.Dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("fbmcpctl status")
	fmt.Println("state:", cfg.State.Dir)
	fmt.Println("databases:", len(cfg.Databases))
	ok, report := posture.Report(cfg)
	fmt.Println("posture_ok:", ok)
	fmt.Print(report)
	fmt.Println("pending:", len(st.Pending()))
	fmt.Println("schedules:", len(st.Schedules()))
	lastNV := "-"
	for _, w := range st.Workflows() {
		if w.Type == "nightly_verify" {
			lastNV = fmt.Sprintf("%s %s %s", w.ID, w.State, w.Message)
		}
	}
	fmt.Println("last_nightly_verify:", lastNV)
	runs := st.ScheduleRuns()
	if n := len(runs); n > 0 {
		r := runs[n-1]
		fmt.Printf("last_schedule_run: %s %s %s %s\n", r.ScheduleID, r.Target, r.Result, r.Reason)
	}
	fmt.Println("note: OS cron must not spawn a second fbmcp (ADR-005/023)")
	return 0
}

func cmdSetup(args []string) int {
	cfgPath := "fbmcp.yaml"
	writePosture := false
	for _, a := range args {
		if a == "--write-posture" {
			writePosture = true
			continue
		}
		if a != "" && a[0] != '-' {
			cfgPath = a
		}
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup: load config:", err)
		fmt.Fprintln(os.Stderr, "generate a skeleton with packaging/fbmcp.yaml.example")
		return 1
	}
	ok, report := posture.Report(cfg)
	fmt.Print(report)
	if !ok {
		fmt.Fprintln(os.Stderr, "setup: posture verify failed")
		return 1
	}
	if writePosture {
		if err := posture.WriteMarker(cfg.State.Dir, "fbmcpctl setup"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("wrote", posture.Marker(cfg.State.Dir))
	}
	fmt.Println("secrets: environment variable, then <secrets.dir>/<NAME>, then the OS keyring (fbmcpctl secret set).")
	var sysdbaRO []string
	for _, d := range cfg.Databases {
		if strings.EqualFold(d.ROUser, "SYSDBA") {
			sysdbaRO = append(sysdbaRO, d.ID)
		}
	}
	if len(sysdbaRO) > 0 {
		fmt.Printf("RO users: %s read as SYSDBA; for production create RO users (fb_user_create + GRANT).\n", strings.Join(sysdbaRO, ", "))
	}
	fmt.Println("self-check: start fbmcp, fb_ping, confirm a Tier-1 tool, fbmcpctl doctor")
	return 0
}

func cmdDoctor(args []string) int {
	if len(args) > 0 && (args[0] == "--remote" || args[0] == "-remote") {
		cfgPath := "fbmcp.yaml"
		if len(args) > 1 {
			cfgPath = args[1]
		}
		return doctorRemote(cfgPath)
	}
	cfgPath := "fbmcp.yaml"
	if len(args) > 0 {
		cfgPath = args[0]
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	code := 0
	ok, report := posture.Report(cfg)
	fmt.Print(report)
	if !ok {
		code = 1
	}
	for _, db := range cfg.Databases {
		if _, err := config.SecretFromEnv(db.AdminSecretEnv); err != nil {
			fmt.Println("FAIL secret", db.ID, db.AdminSecretEnv, err)
			code = 1
		} else {
			fmt.Println("ok secret", db.ID, db.AdminSecretEnv)
		}
		if _, err := config.SecretFromEnv(db.ROSecretEnv); err != nil {
			fmt.Println("FAIL ro secret", db.ID, err)
			code = 1
		}
	}
	st, err := state.Open(cfg.State.Dir)
	if err != nil {
		fmt.Println("FAIL state", err)
		return 1
	}
	fmt.Println("ok state jobs", len(st.Jobs()), "schedules", len(st.Schedules()))
	bus, err := notify.New(cfg.State.Dir, "", "")
	if err == nil {
		_ = bus.Emit(notify.Event{Type: "doctor.ping", Message: "fbmcpctl doctor"})
		fmt.Println("ok k7 local event log")
	}
	if code == 0 {
		fmt.Println("doctor: green")
	} else {
		fmt.Println("doctor: failed")
	}
	return code
}
