package posture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aleks/fbmcp/internal/config"
)

func TestRefuseWithoutMarker(t *testing.T) {
	dir := t.TempDir()
	if Verified(dir) {
		t.Fatal("empty dir should be unverified")
	}
	if err := WriteMarker(dir, "test"); err != nil {
		t.Fatal(err)
	}
	if !Verified(dir) {
		t.Fatal("marker not seen")
	}
}

func TestReportStateDir(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{State: config.State{Dir: dir}}
	ok, text := Report(cfg)
	if !ok {
		t.Fatal(text)
	}
	cfg2 := &config.Config{State: config.State{Dir: filepath.Join(dir, "missing")}}
	ok, _ = Report(cfg2)
	if ok {
		t.Fatal("missing state dir reported ok")
	}
	_ = os.MkdirAll(cfg2.State.Dir, 0o755)
}
