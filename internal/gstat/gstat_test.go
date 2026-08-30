package gstat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/adminexec"
	"github.com/aleks/fbmcp/internal/config"
)

func TestBuildArgsHeader(t *testing.T) {
	args, err := BuildArgs("SYSDBA", `C:\db\EMPLOYEE.FDB`, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-user", "SYSDBA", "-h", `C:\db\EMPLOYEE.FDB`}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("header args = %v, want %v", args, want)
	}
}

func TestBuildArgsRecordsTables(t *testing.T) {
	args, err := BuildArgs("SYSDBA", "/db/emp.fdb", Options{Mode: ModeRecords, Tables: []string{"EMPLOYEE", "JOB"}, System: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-user", "SYSDBA", "-r", "-s", "-t", "EMPLOYEE", "-t", "JOB", "/db/emp.fdb"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("records args = %v, want %v", args, want)
	}
}

func TestBuildArgsValidation(t *testing.T) {
	cases := []struct {
		name string
		o    Options
	}{
		{"bad mode", Options{Mode: "full"}},
		{"tables in header mode", Options{Tables: []string{"EMPLOYEE"}}},
		{"system in header mode", Options{System: true}},
		{"space in name", Options{Mode: ModeRecords, Tables: []string{"BAD NAME"}}},
		{"leading dash", Options{Mode: ModeRecords, Tables: []string{"-x"}}},
		{"quote in name", Options{Mode: ModeRecords, Tables: []string{`q"uote`}}},
		{"semicolon in name", Options{Mode: ModeRecords, Tables: []string{"a;b"}}},
	}
	for _, c := range cases {
		if _, err := BuildArgs("SYSDBA", "db", c.o); err == nil {
			t.Errorf("%s: expected error, got none", c.name)
		}
	}
	many := make([]string, maxTables+1)
	for i := range many {
		many[i] = fmt.Sprintf("T%d", i)
	}
	if _, err := BuildArgs("SYSDBA", "db", Options{Mode: ModeRecords, Tables: many}); err == nil {
		t.Error("table cap: expected error, got none")
	}
	// RDB$ names are legal (system relations with System=true).
	if _, err := BuildArgs("SYSDBA", "db", Options{Mode: ModeRecords, Tables: []string{"RDB$RELATIONS"}, System: true}); err != nil {
		t.Errorf("RDB$ name rejected: %v", err)
	}
}

func TestBinFallback(t *testing.T) {
	dir := t.TempDir()
	if _, err := Bin(config.FBInstance{BinDir: dir}); err == nil {
		t.Fatal("expected error for empty bin_dir")
	}
	if err := os.WriteFile(filepath.Join(dir, "gstat"), []byte{}, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := Bin(config.FBInstance{BinDir: dir}); err != nil || !strings.HasSuffix(got, "gstat") {
		t.Fatalf("fallback bin = %q, err = %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gstat.exe"), []byte{}, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := Bin(config.FBInstance{BinDir: dir}); err != nil || !strings.HasSuffix(got, "gstat.exe") {
		t.Fatalf(".exe bin = %q, err = %v", got, err)
	}
}

func TestRunContract(t *testing.T) {
	// contract test asserts the Windows layout (gstat.exe under bin_dir);
	// skip-with-log on hosts without it (C1 CI policy)
	if _, err := Bin(config.FBInstance{ID: "probe", BinDir: `C:\Firebird5`}); err != nil {
		t.Skip("gstat.exe not available on this host (CI): running the arg/env contract with stubbed runCmd still requires the Windows path layout")
	}
	var gotBin string
	var gotArgs []string
	var gotEnv map[string]string
	orig := runCmd
	runCmd = func(ctx context.Context, bin string, args []string, timeout time.Duration, maxOutput int64, secretEnv map[string]string) adminexec.Result {
		gotBin, gotArgs, gotEnv = bin, args, secretEnv
		return adminexec.Result{Output: "ok", Exit: 0}
	}
	t.Cleanup(func() { runCmd = orig })

	inst := config.FBInstance{ID: "fb5", BinDir: `C:\Firebird5`}
	out, err := Run(context.Background(), inst, "SYSDBA", "s3cret", `C:\db\EMPLOYEE.FDB`, Options{Mode: ModeRecords, Tables: []string{"EMPLOYEE"}})
	if err != nil {
		t.Fatal(err)
	}
	if out != "ok" {
		t.Fatalf("out = %q", out)
	}
	if want := `C:\Firebird5\gstat.exe`; gotBin != want {
		t.Errorf("bin = %q, want %q", gotBin, want)
	}
	if gotEnv["ISC_PASSWORD"] != "s3cret" {
		t.Errorf("ISC_PASSWORD not passed via env")
	}
	for _, a := range gotArgs {
		if a == "s3cret" {
			t.Errorf("password leaked into argv: %v", gotArgs)
		}
	}
}

func TestRunErrorTaxonomy(t *testing.T) {
	orig := runCmd
	t.Cleanup(func() { runCmd = orig })

	// Run resolves the executable before dispatching; give it a fake one.
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "gstat.exe"), []byte{}, 0o755); err != nil {
		t.Fatal(err)
	}
	inst := config.FBInstance{BinDir: binDir}

	runCmd = func(context.Context, string, []string, time.Duration, int64, map[string]string) adminexec.Result {
		return adminexec.Result{Output: "\nUnable to perform operation\n-System privilege USE_GSTAT_UTILITY is missing\n", Exit: 1}
	}
	_, err := Run(context.Background(), inst, "SYSDBA", "p", "db", Options{Mode: ModeRecords})
	if err == nil || !strings.Contains(err.Error(), "USE_GSTAT_UTILITY") || !strings.Contains(err.Error(), "hint:") {
		t.Fatalf("privilege error = %v", err)
	}

	runCmd = func(context.Context, string, []string, time.Duration, int64, map[string]string) adminexec.Result {
		return adminexec.Result{Output: "Your user name and password are not defined", Exit: 1, Err: fmt.Errorf("exit status 1")}
	}
	_, err = Run(context.Background(), inst, "SYSDBA", "p", "db", Options{Mode: ModeRecords})
	if err == nil || !strings.Contains(err.Error(), "hint:") {
		t.Fatalf("auth error = %v", err)
	}

	runCmd = func(context.Context, string, []string, time.Duration, int64, map[string]string) adminexec.Result {
		return adminexec.Result{Output: "", Exit: 0}
	}
	if _, err := Run(context.Background(), inst, "SYSDBA", "p", "db", Options{}); err == nil {
		t.Fatal("empty output: expected error")
	}
}

// TestRunWireRoute: records mode with an instance address attaches over
// the wire first (immune to the calling process holding SQL connections
// to the same database); a wire success never touches the file route.
func TestRunWireRoute(t *testing.T) {
	orig := runCmd
	t.Cleanup(func() { runCmd = orig })
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "gstat.exe"), []byte{}, 0o755); err != nil {
		t.Fatal(err)
	}
	var lastArgs []string
	calls := 0
	runCmd = func(_ context.Context, _ string, args []string, _ time.Duration, _ int64, _ map[string]string) adminexec.Result {
		calls++
		lastArgs = args
		return adminexec.Result{Output: "Analyzing database pages ...\nGstat completion time x"}
	}
	inst := config.FBInstance{ID: "fb5", BinDir: binDir, Addr: "localhost:3055"}
	out, err := Run(context.Background(), inst, "SYSDBA", "p", `C:\db\E.FDB`, Options{Mode: ModeRecords, Tables: []string{"EMPLOYEE"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Analyzing database pages") {
		t.Fatal("wire output not returned")
	}
	if calls != 1 {
		t.Fatalf("expected 1 subprocess call (wire), got %d", calls)
	}
	if want := "localhost/3055:" + `C:\db\E.FDB`; lastArgs[len(lastArgs)-1] != want {
		t.Fatalf("wire target = %q, want %q", lastArgs[len(lastArgs)-1], want)
	}

	// transport failure on the wire → embedded file fallback
	calls = 0
	runCmd = func(_ context.Context, _ string, _ []string, _ time.Duration, _ int64, _ map[string]string) adminexec.Result {
		calls++
		if calls == 1 {
			return adminexec.Result{Output: "Unable to complete network request to host localhost/3055", Exit: 1, Err: fmt.Errorf("exit status 1")}
		}
		return adminexec.Result{Output: "Analyzing database pages ...\nGstat completion time x"}
	}
	if _, err := Run(context.Background(), inst, "SYSDBA", "p", `C:\db\E.FDB`, Options{Mode: ModeRecords}); err != nil {
		t.Fatalf("embedded fallback not taken: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected wire+file attempts, got %d", calls)
	}

	// non-transport wire failure (auth) surfaces directly — no fallback
	calls = 0
	runCmd = func(_ context.Context, _ string, _ []string, _ time.Duration, _ int64, _ map[string]string) adminexec.Result {
		calls++
		return adminexec.Result{Output: "Your user name and password are not defined", Exit: 1, Err: fmt.Errorf("exit status 1")}
	}
	_, err = Run(context.Background(), inst, "SYSDBA", "p", `C:\db\E.FDB`, Options{Mode: ModeRecords})
	if err == nil || !strings.Contains(err.Error(), "hint:") {
		t.Fatalf("wire auth failure must surface: %v", err)
	}
	if calls != 1 {
		t.Fatalf("no file fallback expected on auth failure, got %d calls", calls)
	}
}

// TestRunPluginRetry: one plugin-contention failure (exit 1, no analysis)
// then success on the automatic retry.
func TestRunPluginRetry(t *testing.T) {
	orig := runCmd
	t.Cleanup(func() { runCmd = orig })
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "gstat.exe"), []byte{}, 0o755); err != nil {
		t.Fatal(err)
	}
	calls := 0
	runCmd = func(context.Context, string, []string, time.Duration, int64, map[string]string) adminexec.Result {
		calls++
		if calls == 1 {
			return adminexec.Result{Output: "Error loading plugin MySQLEngine\nGstat completion time x", Exit: 1}
		}
		return adminexec.Result{Output: "Analyzing database pages ...\nEMPLOYEE (131)\nGstat completion time x", Exit: 0}
	}
	out, err := Run(context.Background(), config.FBInstance{BinDir: binDir}, "SYSDBA", "p", "db", Options{Mode: ModeRecords})
	if err != nil {
		t.Fatalf("retry did not recover: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected exactly 2 subprocess calls, got %d", calls)
	}
	if !strings.Contains(out, "EMPLOYEE (131)") {
		t.Fatal("second attempt output not returned")
	}
}

// TestRunPluginNoiseRescue: HQbird plugin load failures make gstat exit 1
// with a complete report — Run must succeed and keep the noise visible.
func TestRunPluginNoiseRescue(t *testing.T) {
	orig := runCmd
	t.Cleanup(func() { runCmd = orig })
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "gstat.exe"), []byte{}, 0o755); err != nil {
		t.Fatal(err)
	}
	full := "Error loading plugin MySQLEngine\n" +
		"Database \"DB\"\nGstat execution time now\n" +
		"Analyzing database pages ...\nEMPLOYEE (131)\n" +
		"Gstat completion time now"
	runCmd = func(context.Context, string, []string, time.Duration, int64, map[string]string) adminexec.Result {
		return adminexec.Result{Output: full, Exit: 1, Err: fmt.Errorf("exit status 1")}
	}
	out, err := Run(context.Background(), config.FBInstance{BinDir: binDir}, "SYSDBA", "p", "db", Options{Mode: ModeRecords})
	if err != nil {
		t.Fatalf("completed report with plugin noise must not fail: %v", err)
	}
	if !strings.Contains(out, "MySQLEngine") || !strings.Contains(out, "EMPLOYEE (131)") {
		t.Fatal("noise/stats must stay in collected output")
	}
	// exit 1 WITHOUT the completion markers is still a failure.
	runCmd = func(context.Context, string, []string, time.Duration, int64, map[string]string) adminexec.Result {
		return adminexec.Result{Output: "Error loading plugin MySQLEngine\n(nothing analyzed)", Exit: 1}
	}
	if _, err := Run(context.Background(), config.FBInstance{BinDir: binDir}, "SYSDBA", "p", "db", Options{Mode: ModeRecords}); err == nil {
		t.Fatal("incomplete report must fail")
	}
}

// TestLive is an opt-in smoke test against a real gstat install:
//
//	FBMCP_GSTAT_LIVE=1 FBMCP_GSTAT_BIN=C:/Firebird5 \
//	FBMCP_GSTAT_DB='C:\...\EMPLOYEE.FDB' FBMCP_DEV_PW=… go test ./internal/gstat -run Live
//
// Also proves the -h contract: header mode succeeds even with a wrong
// password (it reads the file directly, no authentication).
func TestLive(t *testing.T) {
	if os.Getenv("FBMCP_GSTAT_LIVE") == "" {
		t.Skip("set FBMCP_GSTAT_LIVE=1 to run")
	}
	binDir := envOr("FBMCP_GSTAT_BIN", `C:/HQbird/Firebird50`)
	dbPath := envOr("FBMCP_GSTAT_DB", `C:\HQbird\Firebird50\examples\empbuild\EMPLOYEE.FDB`)
	inst := config.FBInstance{ID: "live", BinDir: binDir}

	out, err := Run(context.Background(), inst, "SYSDBA", "wrong-password-on-purpose", dbPath, Options{Mode: ModeHeader})
	if err != nil {
		t.Fatalf("header mode must not need auth: %v", err)
	}
	if !strings.Contains(out, "Database header page information:") {
		t.Fatalf("header output missing header block:\n%s", head(out, 5))
	}

	pass := os.Getenv("FBMCP_DEV_PW")
	if pass == "" {
		t.Skip("FBMCP_DEV_PW unset; header-only live check done")
	}
	out, err = Run(context.Background(), inst, "SYSDBA", pass, dbPath, Options{Mode: ModeRecords, Tables: []string{"EMPLOYEE", "JOB"}})
	if err != nil {
		t.Fatalf("records mode: %v", err)
	}
	if !strings.Contains(out, "Analyzing database pages") || !strings.Contains(out, "EMPLOYEE (") {
		t.Fatalf("records output missing table stats:\n%s", head(out, 5))
	}
	if strings.Contains(out, "SALES (") {
		t.Fatalf("table filter leaked: SALES section present")
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
