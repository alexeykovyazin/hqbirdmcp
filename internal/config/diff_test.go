package config

import (
	"strings"
	"testing"
)

func testSnap() *Config {
	return &Config{
		State:     State{Dir: "/var/lib/fbmcp"},
		Instances: []FBInstance{{ID: "fb5", Addr: "localhost:3055", BinDir: "/opt/fb"}},
		Databases: []Database{{
			ID: "spike5", Instance: "fb5", Path: `C:/data/a.fdb`,
			BackupDir: "C:/bak", WorkDir: "C:/work",
			ROUser: "ro", ROSecretEnv: "RO", AdminUser: "SYSDBA", AdminSecretEnv: "ADM",
		}},
	}
}

func TestSnapshotHashNilEmptyEqual(t *testing.T) {
	a := testSnap()
	b := testSnap()
	b.Identities = []APIIdentity{}
	a.Identities = nil
	if SnapshotHash(a) != SnapshotHash(b) {
		t.Fatal("nil identities must hash equal to empty")
	}
}

func TestCompareNoop(t *testing.T) {
	a := testSnap()
	d := CompareSnapshots(a, testSnap())
	if d.Class != "noop" {
		t.Fatalf("class=%s", d.Class)
	}
}

func TestCompareStateDirRefuse(t *testing.T) {
	a, b := testSnap(), testSnap()
	b.State.Dir = "/other"
	d := CompareSnapshots(a, b)
	if d.Class != "refuse" || !strings.Contains(d.Refuse, "state.dir") {
		t.Fatalf("%+v", d)
	}
}

func TestCompareAddDBImmediate(t *testing.T) {
	a, b := testSnap(), testSnap()
	b.Databases = append(b.Databases, Database{
		ID: "extra", Instance: "fb5", Path: `C:/data/b.fdb`,
		ROUser: "ro", ROSecretEnv: "RO", AdminUser: "SYSDBA", AdminSecretEnv: "ADM",
	})
	d := CompareSnapshots(a, b)
	if d.Class != "apply" || len(d.AddedDBs) != 1 || d.AddedDBs[0] != "extra" {
		t.Fatalf("%+v", d)
	}
	if len(d.DrainIDs) != 0 {
		t.Fatalf("add must not drain: %+v", d)
	}
}

func TestCompareRemoveDrains(t *testing.T) {
	a, b := testSnap(), testSnap()
	b.Databases = nil
	d := CompareSnapshots(a, b)
	if d.Class != "apply" || len(d.RemovedDBs) != 1 || d.RemovedDBs[0] != "spike5" {
		t.Fatalf("%+v", d)
	}
	if len(d.DrainIDs) != 1 || d.DrainIDs[0] != "spike5" {
		t.Fatalf("remove must drain: %+v", d)
	}
}

func TestCompareRenameStrict(t *testing.T) {
	a, b := testSnap(), testSnap()
	b.Databases[0].ID = "renamed"
	d := CompareSnapshots(a, b)
	if d.Renames["spike5"] != "renamed" {
		t.Fatalf("want rename, got %+v", d)
	}
	if len(d.AddedDBs) != 0 || len(d.RemovedDBs) != 0 {
		t.Fatalf("rename leaked add/remove: %+v", d)
	}
}

func TestCompareDuplicatePathRefuse(t *testing.T) {
	a, b := testSnap(), testSnap()
	b.Databases = append(b.Databases, Database{
		ID: "dup", Instance: "fb5", Path: `c:\data\a.fdb`,
		ROUser: "ro", ROSecretEnv: "RO", AdminUser: "SYSDBA", AdminSecretEnv: "ADM",
	})
	d := CompareSnapshots(a, b)
	if d.Class != "refuse" || !strings.Contains(d.Refuse, "duplicate") {
		t.Fatalf("%+v", d)
	}
}

func TestComparePathSwapNotRename(t *testing.T) {
	a := testSnap()
	a.Databases = append(a.Databases, Database{
		ID: "other", Instance: "fb5", Path: `C:/data/b.fdb`,
		ROUser: "ro", ROSecretEnv: "RO", AdminUser: "SYSDBA", AdminSecretEnv: "ADM",
	})
	b := testSnap()
	b.Databases = []Database{
		{ID: "spike5", Instance: "fb5", Path: `C:/data/b.fdb`, ROUser: "ro", ROSecretEnv: "RO", AdminUser: "SYSDBA", AdminSecretEnv: "ADM"},
		{ID: "other", Instance: "fb5", Path: `C:/data/a.fdb`, ROUser: "ro", ROSecretEnv: "RO", AdminUser: "SYSDBA", AdminSecretEnv: "ADM"},
	}
	d := CompareSnapshots(a, b)
	if len(d.Renames) != 0 {
		t.Fatalf("path swap classified as rename: %+v", d)
	}
	if len(d.DrainIDs) < 2 {
		t.Fatalf("path swap should drain both: %+v", d)
	}
}

func TestCompareSecretEnvDrains(t *testing.T) {
	a, b := testSnap(), testSnap()
	b.Databases[0].AdminSecretEnv = "NEW"
	d := CompareSnapshots(a, b)
	if len(d.DrainIDs) != 1 || d.DrainIDs[0] != "spike5" {
		t.Fatalf("%+v", d)
	}
}

func TestCompareInstanceAddrDrainsAllDBs(t *testing.T) {
	a, b := testSnap(), testSnap()
	b.Instances[0].Addr = "localhost:3999"
	d := CompareSnapshots(a, b)
	hasDB, hasInst := false, false
	for _, id := range d.DrainIDs {
		if id == "spike5" {
			hasDB = true
		}
		if id == "fb5" {
			hasInst = true
		}
	}
	if !hasDB || !hasInst {
		t.Fatalf("addr change drain=%v", d.DrainIDs)
	}
}

func TestCompareListenAuthNotify(t *testing.T) {
	a, b := testSnap(), testSnap()
	b.Listen = "10.0.0.5:8443"
	b.TLS = TLS{Cert: "c", Key: "k"}
	b.Identities = []APIIdentity{{Name: "op", KeyEnv: "K", MaxTier: 2}}
	b.Notify.WebhookURL = "https://example"
	d := CompareSnapshots(a, b)
	if !d.Listener || !d.Auth || !d.Notify {
		t.Fatalf("%+v", d)
	}
}

func TestCompareIdentitiesOmittedVsEmpty(t *testing.T) {
	a, b := testSnap(), testSnap()
	a.Identities = nil
	b.Identities = []APIIdentity{}
	d := CompareSnapshots(a, b)
	if d.Class != "noop" {
		t.Fatalf("empty vs nil identities: %+v", d)
	}
}

func TestHandleSwap(t *testing.T) {
	h := NewHandle(testSnap())
	if _, err := h.DB("spike5"); err != nil {
		t.Fatal(err)
	}
	next := testSnap()
	next.Databases[0].ID = "other"
	next.Databases[0].Path = `C:/data/other.fdb`
	h.Swap(next)
	if _, err := h.DB("spike5"); err == nil {
		t.Fatal("old id still present")
	}
	if _, err := h.DB("other"); err != nil {
		t.Fatal(err)
	}
}
