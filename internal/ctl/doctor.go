package ctl

// fbmcp ctl doctor (UX review W1.6): one line per check of a local
// install, right the first time: it makes a missing state folder, reads
// secrets.dir, logs in to every database with the read-only and the admin
// user, and every FAIL line ends with the fix. Secret values are never
// printed.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/dbpool"
	"github.com/aleks/fbmcp/internal/notify"
	"github.com/aleks/fbmcp/internal/posture"
	"github.com/aleks/fbmcp/internal/secrets"
	"github.com/aleks/fbmcp/internal/state"
)

// dbLogin opens one real Firebird connection with the read-only or the
// admin user of a database. A variable so the tests need no Firebird.
var dbLogin = func(ctx context.Context, cfg *config.Config, dbID string, admin bool) error {
	m := dbpool.NewManager(cfg)
	defer m.Close()
	var err error
	if admin {
		_, err = m.AdminPool(ctx, dbID)
	} else {
		_, err = m.ReadPool(ctx, dbID)
	}
	return err
}

func cmdDoctor(args []string) int {
	explicit, args := takeConfigFlag(args)
	remote := false
	for _, a := range args {
		switch {
		case a == "--remote" || a == "-remote":
			remote = true
		case a != "" && a[0] != '-':
			explicit = a
		}
	}
	if remote {
		path, err := resolveConfig(explicit)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return doctorRemote(path)
	}
	cfg, cfgPath, err := loadConfig(explicit)
	if err != nil {
		d := &doctorOut{}
		switch {
		case cfgPath == "": // nothing found; the error says what was tried
			d.fail("config", "", "%v", err)
		case errors.Is(err, fs.ErrNotExist):
			d.fail("config", "give the right file with --config <path>, or set FBMCP_CONFIG", "%v", err)
		default:
			d.fail("config", "correct "+quoteArg(cfgPath)+" (the error names the line)", "%v", err)
		}
		return 1
	}
	secrets.SetDir(cfg.Secrets.Dir)
	d := &doctorOut{cfgPath: cfgPath}
	fmt.Printf("doctor %s\n", cfgPath)
	d.ok("config", "%d databases, %d instances", len(cfg.Databases), len(cfg.Instances))

	stateOK := checkStateDir(d, cfg)
	checkPosture(d, cfg)
	checkSecretsDir(d, cfg)
	var sysdbaRO []string
	for _, db := range cfg.Databases {
		if strings.EqualFold(db.ROUser, "SYSDBA") {
			sysdbaRO = append(sysdbaRO, db.ID)
		}
		checkDatabase(d, cfg, db)
	}
	if len(sysdbaRO) > 0 {
		d.warn("ro-user", "%s read as SYSDBA; for production make a real read-only user (FBMCP_RO, see docs/quickstart-en.md)", strings.Join(sysdbaRO, ", "))
	}
	if stateOK {
		checkState(d, cfg)
	}
	if d.fails == 0 {
		fmt.Printf("doctor: green (%d WARN)\n", d.warns)
		return 0
	}
	fmt.Printf("doctor: %d FAIL, %d WARN\n", d.fails, d.warns)
	return 1
}

// checkStateDir makes a missing state folder: fbmcp needs it before its
// first start, and a missing one was the first error of most installs.
func checkStateDir(d *doctorOut, cfg *config.Config) bool {
	dir := cfg.State.Dir
	if dir == "" {
		d.fail("state", "set state.dir in "+d.cfgName(), "state.dir is empty")
		return false
	}
	if isDir(dir) {
		d.ok("state", "folder %s", dir)
		return true
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		d.fail("state", "mkdir "+quoteArg(dir)+" as the account that runs fbmcp, or correct state.dir in "+d.cfgName(),
			"state folder %s is missing and cannot be made: %v", dir, err)
		return false
	}
	d.ok("state", "folder %s made now", dir)
	if runtime.GOOS != "windows" {
		d.note("state", "if fbmcp runs as another user, give it the folder: sudo chown -R <service user> %s", quoteArg(dir))
	}
	return true
}

// checkPosture shows the posture report in the doctor format. Its state
// folder line is checkStateDir's job.
func checkPosture(d *doctorOut, cfg *config.Config) {
	_, report := posture.Report(cfg)
	dirs := 0
	for _, l := range strings.Split(strings.TrimSpace(report), "\n") {
		switch {
		case strings.HasPrefix(l, "ok: ") && strings.Contains(l, " dir "):
			dirs++ // one line for all backup and work folders
		case strings.HasPrefix(l, "FAIL: "):
			if !strings.Contains(l, "state.dir") {
				d.fail("posture", "", "%s", strings.TrimPrefix(l, "FAIL: "))
			}
		case strings.HasPrefix(l, "WARN: "):
			d.warn("posture", "%s", strings.TrimPrefix(l, "WARN: "))
		case strings.HasPrefix(l, "note: "):
			d.note("posture", "%s", strings.TrimPrefix(l, "note: "))
		case strings.HasPrefix(l, "ok: ") && !strings.Contains(l, "state.dir"):
			d.ok("posture", "%s", strings.TrimPrefix(l, "ok: "))
		}
	}
	if dirs > 0 {
		d.ok("posture", "%d backup and work folders exist", dirs)
	}
}

func checkSecretsDir(d *doctorOut, cfg *config.Config) {
	dir := cfg.Secrets.Dir
	if dir == "" {
		d.note("secrets", "no secrets.dir: passwords come from the environment or the OS keyring of the account that runs fbmcp")
		return
	}
	if !isDir(dir) {
		d.fail("secrets", "fbmcp ctl secret set --dir "+quoteArg(dir)+" <NAME> makes it, or correct secrets.dir in "+d.cfgName(),
			"secrets folder %s does not exist", dir)
		return
	}
	d.ok("secrets", "folder %s", dir)
}

// checkDatabase checks both passwords of one database and logs in with each.
func checkDatabase(d *doctorOut, cfg *config.Config, db config.Database) {
	name := "db:" + db.ID
	for _, role := range []struct {
		label, user, env string
		admin            bool
	}{
		{"read-only", db.ROUser, db.ROSecretEnv, false},
		{"admin", db.AdminUser, db.AdminSecretEnv, true},
	} {
		if role.env == "" {
			d.fail(name, "add "+map[bool]string{false: "ro_secret_env", true: "admin_secret_env"}[role.admin]+" to the database in "+d.cfgName(),
				"no secret name for the %s user", role.label)
			continue
		}
		_, src, err := secrets.Lookup(role.env)
		if err != nil {
			d.fail(name, secretFix(cfg, role.env), "%s password %s not found (environment, secrets folder, keyring)", role.label, role.env)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err = dbLogin(ctx, cfg, db.ID, role.admin)
		cancel()
		if err != nil {
			d.fail(name, loginFix(d, cfg, db, role.user, role.env, err), "%s login as %s fails: %v", role.label, role.user, err)
			continue
		}
		d.ok(name, "%s login as %s works (password from %s)", role.label, role.user, src)
	}
}

func secretFix(cfg *config.Config, env string) string {
	how := "type the password, then Enter and " + eofKeys()
	if cfg.Secrets.Dir != "" {
		return fmt.Sprintf("fbmcp ctl secret set --dir %s %s (%s)", quoteArg(cfg.Secrets.Dir), env, how)
	}
	return fmt.Sprintf("fbmcp ctl secret set %s (%s; this stores it in the keyring of the current account), or set the environment variable %s for fbmcp", env, how, env)
}

func eofKeys() string {
	if runtime.GOOS == "windows" {
		return "Ctrl+Z, Enter"
	}
	return "Ctrl+D"
}

// loginFix names the likely cause of a failed Firebird login.
func loginFix(d *doctorOut, cfg *config.Config, db config.Database, user, env string, err error) string {
	addr := db.Instance
	if inst, ierr := cfg.Instance(db.Instance); ierr == nil {
		addr = inst.Addr
	}
	e := strings.ToLower(err.Error())
	switch {
	case strings.Contains(e, "user name and password") || strings.Contains(e, "login") || strings.Contains(e, "password"):
		return fmt.Sprintf("check the password in %s (%s); if user %s does not exist, create it (docs/quickstart-en.md, read-only user)", env, secretFix(cfg, env), user)
	case strings.Contains(e, "refused") || strings.Contains(e, "timeout") || strings.Contains(e, "no such host") || strings.Contains(e, "deadline"):
		return fmt.Sprintf("start Firebird on %s, or correct the addr of instance %s in %s", addr, db.Instance, d.cfgName())
	case strings.Contains(e, "i/o error") || strings.Contains(e, "no such file") || strings.Contains(e, "not found") || strings.Contains(e, "cannot open"):
		return fmt.Sprintf("correct the path of database %s in %s (the path as the Firebird server sees it)", db.ID, d.cfgName())
	}
	return "try the same login with isql: isql -user " + user + " " + cliTarget(addr, db.Path)
}

// cliTarget is the isql connection string: host/port:path.
func cliTarget(addr, path string) string {
	if host, port, err := net.SplitHostPort(addr); err == nil {
		return quoteArg(host + "/" + port + ":" + path)
	}
	return quoteArg(addr + ":" + path)
}

func checkState(d *doctorOut, cfg *config.Config) {
	st, err := state.Open(cfg.State.Dir)
	if err != nil {
		d.fail("state", "fbmcp ctl repair --from-audit --config "+d.cfgName()+" (moves the bad file aside and rebuilds the schedules)", "state.json: %v", err)
		return
	}
	d.ok("state", "%d jobs, %d schedules, %d pending requests", len(st.Jobs()), len(st.Schedules()), len(st.Pending()))
	if bus, err := notify.New(cfg.State.Dir, "", ""); err == nil {
		if err := bus.Emit(notify.Event{Type: "doctor.ping", Message: "fbmcp ctl doctor"}); err == nil {
			d.ok("events", "test event written to %s", filepath.Join(cfg.State.Dir, "events.jsonl"))
		} else {
			d.warn("events", "cannot write %s: %v (the tray reads it)", filepath.Join(cfg.State.Dir, "events.jsonl"), err)
		}
	}
	if kernelUp(cfg.State.Dir) {
		d.ok("kernel", "fbmcp is running for this state folder")
	} else if strings.TrimSpace(cfg.Listen) != "" {
		d.warn("kernel", "fbmcp is not running; start the service: %s", startService())
	} else {
		d.note("kernel", "fbmcp is not running now: normal for a desktop install, the MCP client starts it")
	}
}
