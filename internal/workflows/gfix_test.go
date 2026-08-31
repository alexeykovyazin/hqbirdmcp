package workflows

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/adminexec"
	"github.com/aleks/fbmcp/internal/config"
)

// TestGfixConnectionString (gate for the HQBird plugin-error class): gfix
// must ALWAYS receive a TCP connection string (localhost/port:path), never
// a bare path — a bare path means a local/embedded attach whose plugin
// scan fails on HQBird installs and silently skips the operation.
func TestGfixConnectionString(t *testing.T) {
	orig := runCmd
	var gotArgs []string
	runCmd = func(ctx context.Context, bin string, args []string, timeout time.Duration, maxOutput int64, secretEnv map[string]string) adminexec.Result {
		gotArgs = args
		return adminexec.Result{Output: "ok", Exit: 0}
	}
	t.Cleanup(func() { runCmd = orig })

	inst := config.FBInstance{ID: "fb5", Addr: "localhost:3055", BinDir: `C:\HQbird\Firebird50`}
	dbPath := `C:\HQbirdData\output\fbmcp-spike\spike_FB5.0.fdb`

	if err := GfixShutdown(context.Background(), inst, dbPath, "SYSDBA", "s3cret", "force", time.Second); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	last := gotArgs[len(gotArgs)-1]
	if !strings.HasPrefix(last, "localhost/3055:") {
		t.Fatalf("shutdown target = %q, want localhost/3055:<path>", last)
	}
	if !strings.HasSuffix(last, `spike_FB5.0.fdb`) {
		t.Fatalf("shutdown target lost the database path: %q", last)
	}

	if err := GfixOnline(context.Background(), inst, dbPath, "SYSDBA", "s3cret"); err != nil {
		t.Fatalf("online: %v", err)
	}
	if last = gotArgs[len(gotArgs)-1]; !strings.HasPrefix(last, "localhost/3055:") {
		t.Fatalf("online target = %q, want localhost/3055:<path>", last)
	}

	// default port when the instance addr carries none
	inst2 := config.FBInstance{ID: "fb", Addr: "localhost", BinDir: `C:\FB`}
	if err := GfixOnline(context.Background(), inst2, dbPath, "SYSDBA", "s3cret"); err != nil {
		t.Fatalf("online (default port): %v", err)
	}
	if last = gotArgs[len(gotArgs)-1]; !strings.HasPrefix(last, "localhost/3050:") {
		t.Fatalf("default-port target = %q, want localhost/3050:<path>", last)
	}
}
