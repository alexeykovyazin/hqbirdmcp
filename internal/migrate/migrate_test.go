package migrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFileUpDown(t *testing.T) {
	content := "CREATE TABLE A (N INT);\nINSERT INTO A VALUES (1);\n-- @down\nDROP TABLE A;\n"
	m, err := ParseFile("001_init.sql", content)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != 1 || m.Name != "001_init.sql" {
		t.Errorf("version/name = %d/%s", m.Version, m.Name)
	}
	up := Statements(m.Up)
	if len(up) != 2 || !strings.HasPrefix(up[0], "CREATE TABLE A") || !strings.HasPrefix(up[1], "INSERT INTO A") {
		t.Errorf("up statements = %v", up)
	}
	down := Statements(m.Down)
	if len(down) != 1 || !strings.HasPrefix(down[0], "DROP TABLE A") {
		t.Errorf("down statements = %v", down)
	}
	if len(m.Checksum) != 64 {
		t.Errorf("checksum = %q", m.Checksum)
	}
}

func TestParseFileNoDown(t *testing.T) {
	m, err := ParseFile("010_baseline_only.sql", "CREATE TABLE B (N INT);")
	if err != nil {
		t.Fatal(err)
	}
	if m.HasDown() {
		t.Error("no down expected")
	}
	if m.Version != 10 {
		t.Errorf("version = %d", m.Version)
	}
}

func TestParseFileRejects(t *testing.T) {
	for name, content := range map[string]string{
		"no version":   "CREATE TABLE X (N INT);",
		"bad ext":      "CREATE TABLE X (N INT);",
		"empty up":     "-- @down\nDROP TABLE X;",
		"zero version": "CREATE TABLE X (N INT);",
	} {
		file := "init.sql"
		switch name {
		case "bad ext":
			file = "001_init.txt"
		case "zero version":
			file = "000_zero.sql"
		}
		if _, err := ParseFile(file, content); err == nil {
			t.Errorf("%s: should fail", name)
		}
	}
}

func TestChecksumStableAndSensitive(t *testing.T) {
	a, _ := ParseFile("001_a.sql", "CREATE TABLE A (N INT);")
	b, _ := ParseFile("001_a.sql", "CREATE TABLE A (N INT);")
	if a.Checksum != b.Checksum {
		t.Error("identical content must produce an identical checksum")
	}
	c, _ := ParseFile("001_a.sql", "CREATE TABLE A (M INT);")
	if a.Checksum == c.Checksum {
		t.Error("different content must produce a different checksum")
	}
	// whitespace is content: an edited file (even reformatting) must not
	// reuse the old confirmation/history entry
	d, _ := ParseFile("001_a.sql", "CREATE TABLE A (N INT); ")
	if a.Checksum == d.Checksum {
		t.Error("whitespace-only edit must change the checksum")
	}
}

func TestLoadDirOrdersAndDupDetects(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("002_second.sql", "CREATE TABLE B (N INT);")
	write("001_first.sql", "CREATE TABLE A (N INT);")
	write("README.md", "not a migration")
	migs, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(migs) != 2 || migs[0].Version != 1 || migs[1].Version != 2 {
		t.Fatalf("migs = %+v", migs)
	}
	write("001_duplicate.sql", "CREATE TABLE C (N INT);")
	if _, err := LoadDir(dir); err == nil || !strings.Contains(err.Error(), "duplicate version") {
		t.Errorf("duplicate version should fail: %v", err)
	}
}

func TestPending(t *testing.T) {
	migs := []Migration{
		{Version: 1, Name: "001_a.sql", Checksum: "aa"},
		{Version: 2, Name: "002_b.sql", Checksum: "bb"},
		{Version: 3, Name: "003_c.sql", Checksum: "cc"},
	}
	p, err := Pending(migs, []Applied{{Version: 1, Name: "001_a.sql", Checksum: "aa"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 2 || p[0].Version != 2 || p[1].Version != 3 {
		t.Fatalf("pending = %+v", p)
	}

	// tamper: applied checksum differs from file
	_, err = Pending(migs, []Applied{{Version: 1, Name: "001_a.sql", Checksum: "zz"}})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("tamper should fail: %v", err)
	}

	// history references a file no longer present
	_, err = Pending(migs, []Applied{{Version: 9, Name: "009_x.sql", Checksum: "xx"}})
	if err == nil || !strings.Contains(err.Error(), "not present in the migrations dir") {
		t.Errorf("orphan history should fail: %v", err)
	}

	// gap: pending below applied
	_, err = Pending(migs, []Applied{{Version: 3, Name: "003_c.sql", Checksum: "cc"}})
	if err == nil || !strings.Contains(err.Error(), "in order") {
		t.Errorf("out-of-order pending should fail: %v", err)
	}
}

func TestManifestJSON(t *testing.T) {
	migs := []Migration{
		{Version: 1, Name: "001_a.sql", Checksum: "aa"},
		{Version: 2, Name: "002_b.sql", Checksum: "bb"},
	}
	a := ManifestJSON(false, migs)
	b := ManifestJSON(false, migs)
	if a != b {
		t.Error("manifest must be canonical/stable")
	}
	if !strings.Contains(a, `"version":1`) || !strings.Contains(a, `"checksum":"bb"`) || !strings.Contains(a, `"baseline":false`) {
		t.Errorf("manifest = %s", a)
	}
	if ManifestJSON(true, migs) == a {
		t.Error("baseline flag must change the manifest")
	}
}
