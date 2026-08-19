package main

import (
	"testing"

	"github.com/aleks/fbmcp/internal/backupsvc"
	"github.com/aleks/fbmcp/internal/lockout"
	"github.com/aleks/fbmcp/internal/state"
)

func TestC23LockoutReattack(t *testing.T) {
	if err := lockout.DropUser("Admin", "ro", "sysdba"); err == nil {
		t.Fatal("mixed-case SYSDBA dropped")
	}
	if err := lockout.DropUser("Admin", "ro", "ADMIN"); err == nil {
		t.Fatal("operating identity dropped")
	}
	if err := lockout.Revoke("Admin", "reader", "SYSDBA"); err == nil {
		t.Fatal("SYSDBA revoke")
	}
}

func TestC23SelfKillGuard(t *testing.T) {
	if !isOwnAttachment(42, 42) {
		t.Fatal("must refuse own attachment")
	}
	if isOwnAttachment(42, 43) {
		t.Fatal("foreign attachment treated as own")
	}
	if isOwnAttachment(0, 0) {
		t.Fatal("unset own id must not blanket-refuse 0")
	}
}

func TestC23NbackupGap(t *testing.T) {
	st, _ := state.Open(t.TempDir())
	if _, ok := st.LatestNBackup("spike5", 0); ok {
		t.Fatal("empty catalog claimed level 0")
	}
	_ = backupsvc.NewCatalog(st).RegisterKind("spike5", "/x.nbk", false, "nbackup", 0)
	if _, ok := st.LatestNBackup("spike5", 0); !ok {
		t.Fatal("level 0 missing")
	}
	if _, ok := st.LatestNBackup("spike5", 1); ok {
		t.Fatal("level 1 present without taking it")
	}
}

func TestC23TraceTemplateInjection(t *testing.T) {
	c := &backupsvc.Client{}
	if _, err := c.StartTrace(t.Context(), "x", "database { enabled = true }", nil); err == nil {
		t.Fatal("raw config accepted as template")
	}
}
