package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/gate"
	"github.com/aleks/fbmcp/internal/jobs"
	"github.com/aleks/fbmcp/internal/policy"
	"github.com/aleks/fbmcp/internal/state"
	"github.com/aleks/fbmcp/internal/statetest"
)

func testCfg(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	bak := filepath.Join(dir, "bak")
	work := filepath.Join(dir, "work")
	_ = os.MkdirAll(bak, 0o755)
	_ = os.MkdirAll(work, 0o755)
	return &config.Config{
		State:     config.State{Dir: dir},
		Instances: []config.FBInstance{{ID: "fb5", Addr: "localhost:3055", BinDir: `C:/HQbird/Firebird50`}},
		Databases: []config.Database{{
			ID: "spike5", Instance: "fb5", Path: filepath.Join(dir, "x.fdb"),
			BackupDir: bak, WorkDir: work,
			ROUser: "ro", ROSecretEnv: "X", AdminUser: "SYSDBA", AdminSecretEnv: "Y",
		}},
	}
}

func newTestGT(t *testing.T) *gatedTools {
	t.Helper()
	cfg := testCfg(t)
	st, err := state.Open(cfg.State.Dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_ = st.AddWindow(state.Window{Database: "spike5", From: now.Add(-time.Hour), To: now.Add(time.Hour)})
	aud, err := audit.Open(cfg.State.Dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { aud.Close() })
	facts := statetest.StubFacts{"engine_version": "5.0", "verified_backup_exists": true, "backup_freshness": 1.0}
	eng := policy.New(toolMeta, facts, st)
	runner := jobs.NewRunner(st)
	t.Cleanup(func() { runner.Close() })
	return &gatedTools{
		cfg: config.NewHandle(cfg), eng: eng, g: gate.New(st, aud), runner: runner, aud: aud, st: st,
		execs: map[string]executor{}, args: map[string]map[string]any{},
	}
}

type spyExec struct{ n atomic.Int64 }

func (s *spyExec) wrap() executor {
	return func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
		s.n.Add(1)
		return "spy-ran", nil
	}
}
