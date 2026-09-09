package executor

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/dbpool"
	"github.com/aleks/fbmcp/internal/killpoint"
)

// P1.2 (test_plan): Prepare is pure (classify-driven); the Exec paths run
// against a fake database/sql driver registered under dbpool's (now
// overridable) driver name.

func TestPrepareRejectsReads(t *testing.T) {
	_, err := Prepare("SELECT 1 FROM RDB$DATABASE")
	if err == nil || !strings.Contains(err.Error(), "fb_query") {
		t.Fatalf("Prepare(SELECT) = %v, want fb_query routing", err)
	}
}

func TestPrepareRejectsTier3(t *testing.T) {
	_, err := Prepare("DROP DATABASE X")
	if err == nil || !strings.Contains(err.Error(), "Tier-3") {
		t.Fatalf("Prepare(DROP DATABASE) = %v, want Tier-3 refusal", err)
	}
}

func TestPrepareClassifiesDDLAndDML(t *testing.T) {
	p, err := Prepare("INSERT INTO T VALUES (1); CREATE TABLE X (N INT)")
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasDDL {
		t.Fatal("mixed script must be per-statement (HasDDL)")
	}
	if p.MaxTier < 1 {
		t.Fatalf("MaxTier=%d", p.MaxTier)
	}
	pure, err := Prepare("UPDATE T SET A = 1")
	if err != nil {
		t.Fatal(err)
	}
	if pure.HasDDL {
		t.Fatal("DML-only script must be atomic")
	}
}

// --- fake driver ---

type recStmt struct {
	sql     string
	commits int
}

type fakeRecorder struct {
	mu      sync.Mutex
	execed  []recStmt
	FailSQL string // ExecContext fails for statements containing this
}

func (r *fakeRecorder) snapshot() []recStmt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recStmt(nil), r.execed...)
}

var fakeRegOnce sync.Once

// currentRec is swapped by each test before it drives Exec; the driver
// reads it at call time (sql.Register is global, so the recorder cannot be
// baked into the registration).
var currentRec atomic.Pointer[fakeRecorder]

type fakeDriver struct{}

func (d *fakeDriver) Open(string) (driver.Conn, error) { return &fakeConn{}, nil }

type fakeConn struct{}

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("prepare not used by executor")
}
func (c *fakeConn) Close() error { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *fakeConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return &fakeTx{}, nil
}

func (c *fakeConn) Ping(ctx context.Context) error { return nil }

func (c *fakeConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	rec := currentRec.Load()
	rec.mu.Lock()
	if rec.FailSQL != "" && strings.Contains(query, rec.FailSQL) {
		rec.mu.Unlock()
		return nil, errors.New("boom: engine refused")
	}
	rec.execed = append(rec.execed, recStmt{sql: query})
	rec.mu.Unlock()
	return driver.RowsAffected(1), nil
}

type fakeTx struct{}

func (t *fakeTx) Commit() error {
	rec := currentRec.Load()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i := range rec.execed {
		if rec.execed[i].commits == 0 {
			rec.execed[i].commits = 1
		}
	}
	return nil
}
func (t *fakeTx) Rollback() error { return nil }

func fakeManager(t *testing.T, rec *fakeRecorder) *dbpool.Manager {
	t.Helper()
	dbpool.SetDriverName("fbktest")
	t.Cleanup(func() { dbpool.SetDriverName("firebirdsql") })
	fakeRegOnce.Do(func() { sql.Register("fbktest", &fakeDriver{}) })
	currentRec.Store(rec)
	t.Setenv("FBMCP_T_PW", "pw")
	cfg := &config.Config{
		State:     config.State{Dir: t.TempDir()},
		Instances: []config.FBInstance{{ID: "fbk", Addr: "127.0.0.1:3000", BinDir: "."}},
		Databases: []config.Database{{
			ID: "spike5", Instance: "fbk", Path: "/data/spike5.fdb",
			ROUser: "SYSDBA", ROSecretEnv: "FBMCP_T_PW",
			AdminUser: "SYSDBA", AdminSecretEnv: "FBMCP_T_PW",
		}},
	}
	return dbpool.NewManager(cfg)
}

func TestExecAtomicRollsBackOnFailure(t *testing.T) {
	rec := &fakeRecorder{FailSQL: "FAIL_ME"}
	m := fakeManager(t, rec)
	s := &Service{Pools: m}
	p, err := Prepare("INSERT INTO T VALUES (1); INSERT INTO T VALUES (FAIL_ME); INSERT INTO T VALUES (3)")
	if err != nil {
		t.Fatal(err)
	}
	if p.HasDDL {
		t.Fatal("DML-only script must take the atomic path")
	}
	_, err = s.Exec(context.Background(), "spike5", p, nil)
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("atomic failure error: %v", err)
	}
	var commits int
	for _, e := range rec.snapshot() {
		commits += e.commits
	}
	if commits != 0 {
		t.Fatalf("atomic path committed despite failure (%d)", commits)
	}
}

func TestExecPerStatementReportsPartialApply(t *testing.T) {
	rec := &fakeRecorder{FailSQL: "FAIL_ME"}
	m := fakeManager(t, rec)
	s := &Service{Pools: m}
	p, err := Prepare("CREATE TABLE A (N INT); CREATE TABLE FAIL_ME (N INT); CREATE TABLE B (N INT)")
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasDDL {
		t.Fatal("DDL script must take the per-statement path")
	}
	_, err = s.Exec(context.Background(), "spike5", p, nil)
	if err == nil || !strings.Contains(err.Error(), "PARTIALLY APPLIED: 1 of 3") {
		t.Fatalf("per-statement failure error: %v", err)
	}
	var commits int
	for _, e := range rec.snapshot() {
		commits += e.commits
	}
	if commits != 1 { // exactly the statement before the failure
		t.Fatalf("per-statement commits=%d, want 1", commits)
	}
}

func TestExecAtomicCommitsAll(t *testing.T) {
	rec := &fakeRecorder{}
	m := fakeManager(t, rec)
	s := &Service{Pools: m}
	p, err := Prepare("INSERT INTO T VALUES (1); INSERT INTO T VALUES (2)")
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Exec(context.Background(), "spike5", p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "committed atomically") {
		t.Fatalf("report: %q", out)
	}
	snap := rec.snapshot()
	if len(snap) != 2 || snap[0].commits != 1 || snap[1].commits != 1 {
		t.Fatalf("commit bookkeeping: %+v", snap)
	}
}

func TestExecPreCommitKillpointBlocksCommit(t *testing.T) {
	rec := &fakeRecorder{}
	m := fakeManager(t, rec)
	s := &Service{Pools: m}
	p, err := Prepare("INSERT INTO T VALUES (1)")
	if err != nil {
		t.Fatal(err)
	}
	kpDir := t.TempDir()
	t.Setenv("FBMCP_KILLPOINT_DIR", kpDir)
	killpoint.SetEnabled(map[string]bool{"exec.pre-commit": true})
	t.Cleanup(func() { killpoint.SetEnabled(nil) })

	done := make(chan error, 1)
	go func() {
		_, err := s.Exec(context.Background(), "spike5", p, nil)
		done <- err
	}()
	ready := filepath.Join(kpDir, "exec.pre-commit.ready")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("exec.pre-commit never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, e := range rec.snapshot() {
		if e.commits != 0 {
			t.Fatal("committed before the checkpoint was released")
		}
	}
	if err := os.WriteFile(filepath.Join(kpDir, "exec.pre-commit.release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("exec after release: %v", err)
	}
	snap := rec.snapshot()
	if len(snap) != 1 || snap[0].commits != 1 {
		t.Fatalf("post-release commit bookkeeping: %+v", snap)
	}
}

func TestExecNoPoolFails(t *testing.T) {
	s := &Service{}
	p, err := Prepare("INSERT INTO T VALUES (1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec(context.Background(), "spike5", p, nil); err == nil || !strings.Contains(err.Error(), "no admin pool") {
		t.Fatalf("nil-pool exec: %v", err)
	}
}

func TestImpactRendersWithoutPools(t *testing.T) {
	var nilSvc *Service
	p, err := Prepare("UPDATE T SET A = 1")
	if err != nil {
		t.Fatal(err)
	}
	out := nilSvc.Impact(context.Background(), "", p)
	if !strings.Contains(out, "single transaction") || !strings.Contains(out, "confirmation channels") {
		t.Fatalf("impact: %q", out)
	}
	ddl, err := Prepare("CREATE TABLE X (N INT)")
	if err != nil {
		t.Fatal(err)
	}
	out = nilSvc.Impact(context.Background(), "", ddl)
	if !strings.Contains(out, "per-statement commits") {
		t.Fatalf("ddl impact: %q", out)
	}
}
