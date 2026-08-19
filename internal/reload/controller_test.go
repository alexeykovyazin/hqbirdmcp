package reload

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/configedit"
)

func writeYAML(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "fbmcp.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func baseYAML(stateDir string) string {
	return `
state: {dir: ` + stateDir + `}
instances:
  - id: fb5
    addr: localhost:3055
    bin_dir: /opt/fb
databases:
  - id: spike5
    instance: fb5
    path: /data/a.fdb
    backup_dir: /bak
    work_dir: /work
    ro_user: ro
    ro_secret_env: RO
    admin_user: SYSDBA
    admin_secret_env: ADM
`
}

func TestApplyNoopAndAdd(t *testing.T) {
	dir := t.TempDir()
	st := filepath.Join(dir, "state")
	_ = os.MkdirAll(st, 0o755)
	p := writeYAML(t, dir, baseYAML(st))
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	h := config.NewHandle(cfg)
	var closed []string
	c := New(h, Hooks{
		CloseDB: func(ids []string) { closed = append(closed, ids...) },
	})
	r, err := c.Apply("test", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Class != "noop" {
		t.Fatalf("first apply of identical file: %+v", r)
	}

	cfg2, _ := config.Load(p)
	cfg2.Databases = append(cfg2.Databases, config.Database{
		ID: "extra", Instance: "fb5", Path: "/data/b.fdb",
		BackupDir: "/bak", WorkDir: "/work",
		ROUser: "ro", ROSecretEnv: "RO", AdminUser: "SYSDBA", AdminSecretEnv: "ADM",
	})
	if err := config.RegisterDatabase(p, cfg2.Databases[1]); err != nil {
		t.Fatal(err)
	}
	r, err = c.Apply("fb_db_register", "jself")
	if err != nil {
		t.Fatal(err)
	}
	if r.Class != "apply" || len(r.Diff.AddedDBs) != 1 {
		t.Fatalf("%+v", r)
	}
	if _, err := h.DB("extra"); err != nil {
		t.Fatal(err)
	}
	if len(closed) != 0 {
		t.Fatalf("add closed pools: %v", closed)
	}
}

func TestApplyRefusesStateDir(t *testing.T) {
	dir := t.TempDir()
	st := filepath.Join(dir, "state")
	_ = os.MkdirAll(st, 0o755)
	p := writeYAML(t, dir, baseYAML(st))
	cfg, _ := config.Load(p)
	h := config.NewHandle(cfg)
	c := New(h, Hooks{})
	next := strings.Replace(baseYAML(st), st, filepath.Join(dir, "other"), 1)
	if err := os.WriteFile(p, []byte(next), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := c.Apply("test", "")
	if err == nil || r.Class != "refuse" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := h.DB("spike5"); err != nil {
		t.Fatal("old snapshot lost")
	}
}

func TestApplyDrainTimeoutKeepsOld(t *testing.T) {
	dir := t.TempDir()
	st := filepath.Join(dir, "state")
	_ = os.MkdirAll(st, 0o755)
	p := writeYAML(t, dir, baseYAML(st))
	cfg, _ := config.Load(p)
	h := config.NewHandle(cfg)
	c := New(h, Hooks{
		WaitIdle: func(ctx context.Context, id, except string) error {
			return context.DeadlineExceeded
		},
	}).WithTimeout(20 * time.Millisecond)
	// remove db
	body := strings.Replace(baseYAML(st), "databases:\n  - id: spike5\n    instance: fb5\n    path: /data/a.fdb\n    backup_dir: /bak\n    work_dir: /work\n    ro_user: ro\n    ro_secret_env: RO\n    admin_user: SYSDBA\n    admin_secret_env: ADM\n", "databases: []\n", 1)
	_ = os.WriteFile(p, []byte(body), 0o644)
	r, err := c.Apply("test", "")
	if err == nil || r.Class != "refuse" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := h.DB("spike5"); err != nil {
		t.Fatal("must keep old cfg")
	}
}

func TestApplyExcludesSelfJob(t *testing.T) {
	dir := t.TempDir()
	st := filepath.Join(dir, "state")
	_ = os.MkdirAll(st, 0o755)
	p := writeYAML(t, dir, baseYAML(st))
	cfg, _ := config.Load(p)
	h := config.NewHandle(cfg)
	var waited []string
	c := New(h, Hooks{
		WaitIdle: func(ctx context.Context, id, except string) error {
			waited = append(waited, except)
			if except != "jself" {
				t.Errorf("except=%q", except)
			}
			return nil
		},
		CloseDB: func(ids []string) {},
	})
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(body), "admin_secret_env: ADM", "admin_secret_env: NEW", 1)
	if err := configedit.AtomicWrite(p, updated); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply("test", "jself"); err != nil {
		t.Fatal(err)
	}
	if len(waited) == 0 {
		t.Fatal("expected wait with except")
	}
}

func TestApplySelfJobAddNoWait(t *testing.T) {
	dir := t.TempDir()
	st := filepath.Join(dir, "state")
	_ = os.MkdirAll(st, 0o755)
	p := writeYAML(t, dir, baseYAML(st))
	cfg, _ := config.Load(p)
	h := config.NewHandle(cfg)
	waited := false
	c := New(h, Hooks{
		WaitIdle: func(ctx context.Context, id, except string) error {
			waited = true
			return context.DeadlineExceeded
		},
	})
	db := config.Database{
		ID: "extra", Instance: "fb5", Path: "/data/b.fdb",
		BackupDir: "/bak", WorkDir: "/work",
		ROUser: "ro", ROSecretEnv: "RO", AdminUser: "SYSDBA", AdminSecretEnv: "ADM",
	}
	if err := config.RegisterDatabase(p, db); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply("fb_db_register", "j-fb5-register"); err != nil {
		t.Fatal(err)
	}
	if waited {
		t.Fatal("add-only must not wait")
	}
	if _, err := h.DB("extra"); err != nil {
		t.Fatal(err)
	}
}

func TestWatcherHashNoop(t *testing.T) {
	dir := t.TempDir()
	st := filepath.Join(dir, "state")
	_ = os.MkdirAll(st, 0o755)
	p := writeYAML(t, dir, baseYAML(st))
	cfg, _ := config.Load(p)
	h := config.NewHandle(cfg)
	var mu sync.Mutex
	n := 0
	c := New(h, Hooks{
		CloseDB: func(ids []string) {
			mu.Lock()
			n++
			mu.Unlock()
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.Watch(ctx); err != nil {
		t.Fatal(err)
	}
	if err := configedit.AtomicWrite(p, baseYAML(st)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(900 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if n != 0 {
		t.Fatalf("same content should noop, close called %d", n)
	}
}

func TestApplyRemoveCloses(t *testing.T) {
	dir := t.TempDir()
	st := filepath.Join(dir, "state")
	_ = os.MkdirAll(st, 0o755)
	p := writeYAML(t, dir, baseYAML(st))
	cfg, _ := config.Load(p)
	h := config.NewHandle(cfg)
	var closed []string
	c := New(h, Hooks{
		WaitIdle: func(ctx context.Context, id, except string) error { return nil },
		CloseDB:  func(ids []string) { closed = append(closed, ids...) },
	})
	body := strings.Replace(baseYAML(st), "databases:\n  - id: spike5\n    instance: fb5\n    path: /data/a.fdb\n    backup_dir: /bak\n    work_dir: /work\n    ro_user: ro\n    ro_secret_env: RO\n    admin_user: SYSDBA\n    admin_secret_env: ADM\n", "databases: []\n", 1)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := c.Apply("test", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Class != "apply" || len(r.Diff.RemovedDBs) != 1 {
		t.Fatalf("%+v", r)
	}
	if len(closed) != 1 || closed[0] != "spike5" {
		t.Fatalf("CloseDB=%v", closed)
	}
	if _, err := h.DB("spike5"); err == nil {
		t.Fatal("removed id still in handle")
	}
}

func TestApplySecretEnvCloses(t *testing.T) {
	dir := t.TempDir()
	st := filepath.Join(dir, "state")
	_ = os.MkdirAll(st, 0o755)
	p := writeYAML(t, dir, baseYAML(st))
	cfg, _ := config.Load(p)
	h := config.NewHandle(cfg)
	var closed []string
	c := New(h, Hooks{
		WaitIdle: func(ctx context.Context, id, except string) error { return nil },
		CloseDB:  func(ids []string) { closed = append(closed, ids...) },
	})
	body, _ := os.ReadFile(p)
	if err := configedit.AtomicWrite(p, strings.Replace(string(body), "admin_secret_env: ADM", "admin_secret_env: NEW", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply("test", ""); err != nil {
		t.Fatal(err)
	}
	if len(closed) != 1 || closed[0] != "spike5" {
		t.Fatalf("CloseDB=%v", closed)
	}
}

func TestApplyCheckRemoteRefuse(t *testing.T) {
	dir := t.TempDir()
	st := filepath.Join(dir, "state")
	_ = os.MkdirAll(st, 0o755)
	p := writeYAML(t, dir, baseYAML(st))
	cfg, _ := config.Load(p)
	h := config.NewHandle(cfg)
	c := New(h, Hooks{})
	body, _ := os.ReadFile(p)
	next := string(body) + "\nlisten: 127.0.0.1:8443\ntls:\n  cert: c\n  key: k\nidentities:\n  - name: op\n    key_env: K\n    max_tier: 2\n"
	if err := os.WriteFile(p, []byte(next), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := c.Apply("test", "")
	if err == nil || r.Class != "refuse" {
		t.Fatalf("want ADR-022 refuse, got %+v %v", r, err)
	}
	if h.Current().Listen != "" {
		t.Fatal("listen applied despite refuse")
	}
}

func TestWatcherAppliesChange(t *testing.T) {
	dir := t.TempDir()
	st := filepath.Join(dir, "state")
	_ = os.MkdirAll(st, 0o755)
	p := writeYAML(t, dir, baseYAML(st))
	cfg, _ := config.Load(p)
	h := config.NewHandle(cfg)
	var mu sync.Mutex
	var closed []string
	c := New(h, Hooks{
		WaitIdle: func(ctx context.Context, id, except string) error { return nil },
		CloseDB: func(ids []string) {
			mu.Lock()
			closed = append(closed, ids...)
			mu.Unlock()
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.Watch(ctx); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(p)
	if err := configedit.AtomicWrite(p, strings.Replace(string(body), "admin_secret_env: ADM", "admin_secret_env: NEW", 1)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(closed)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(closed) == 0 {
		t.Fatal("watcher did not apply content change")
	}
	db, err := h.DB("spike5")
	if err != nil || db.AdminSecretEnv != "NEW" {
		t.Fatalf("live snapshot not updated: %+v %v", db, err)
	}
}
