package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDBAReadRoutes(t *testing.T) {
	var seen []string
	cl, _ := newTestAgent(t, "t", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/dba/tools":
			_, _ = w.Write([]byte(`{"tools":[{"tool_id":"t1","tool":"gbak"}]}`))
		case "/v1/dba/ops":
			_, _ = w.Write([]byte(`{"ops":[{"op":"db_sweep","title":"Sweep","class":"normal"}],"groups":["maint"]}`))
		case "/v1/dba/database":
			_, _ = w.Write([]byte(`{"database":"` + r.URL.Query().Get("db") + `","exists":true}`))
		case "/v1/dba/tables":
			b, _ := io.ReadAll(r.Body)
			_, _ = w.Write([]byte(`{"echo":` + string(b) + `,"tables":[]}`))
		case "/v1/dba/runs":
			_, _ = w.Write([]byte(`{"runs":[]}`))
		case "/v1/dba/runs/r1":
			if r.Method == http.MethodDelete {
				_, _ = w.Write([]byte(`{"run_id":"r1","state":"cancelled"}`))
				return
			}
			_, _ = w.Write([]byte(`{"run_id":"r1","op":"db_sweep","state":"done","step_count":1}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not found"}}`))
		}
	}))
	ctx := context.Background()
	if raw, err := cl.DBATools(ctx); err != nil || !strings.Contains(string(raw), "gbak") {
		t.Fatalf("tools: %v %s", err, raw)
	}
	if _, err := cl.DBAOps(ctx, `C:\db\a.fdb`); err != nil {
		t.Fatal(err)
	}
	if title, err := cl.DBAOpTitle(ctx, "db_sweep"); err != nil || title != "Sweep" {
		t.Fatalf("title: %v %q", err, title)
	}
	if raw, err := cl.DBADatabase(ctx, "/data/a.fdb"); err != nil || !strings.Contains(string(raw), "/data/a.fdb") {
		t.Fatalf("database: %v %s", err, raw)
	}
	if raw, err := cl.DBATables(ctx, "/data/a.fdb"); err != nil || !strings.Contains(string(raw), `"database":"/data/a.fdb"`) {
		t.Fatalf("tables: %v %s", err, raw)
	}
	if _, err := cl.GetDBARuns(ctx, DBARunFilter{Active: true, Op: "db_sweep", Database: "/data/a.fdb", Limit: 5}); err != nil {
		t.Fatal(err)
	}
	run, _, err := cl.GetDBARun(ctx, "r1")
	if err != nil || run.State != DBAStateDone || !run.Final() {
		t.Fatalf("run: %v %+v", err, run)
	}
	if raw, err := cl.CancelDBARun(ctx, "r1"); err != nil || !strings.Contains(string(raw), "cancelled") {
		t.Fatalf("cancel: %v %s", err, raw)
	}
	if _, _, err := cl.GetDBARun(ctx, "nope"); err == nil || !strings.Contains(err.Error(), "fbagent not-found") {
		t.Fatalf("unknown run: %v", err)
	}
	want := []string{
		"GET /v1/dba/ops?db=C%3A%5Cdb%5Ca.fdb",
		"GET /v1/dba/runs?active=1&db=%2Fdata%2Fa.fdb&limit=5&op=db_sweep",
		"DELETE /v1/dba/runs/r1?",
	}
	all := strings.Join(seen, "\n")
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Fatalf("request %q not seen in:\n%s", w, all)
		}
	}
}

func TestDBACheckAndRun(t *testing.T) {
	var runBody DBARequest
	cl, _ := newTestAgent(t, "t", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/dba/ops/db_shutdown/check":
			_, _ = w.Write([]byte(`{"op":"db_shutdown","class":"disruptive","database":"/d/a.fdb","checks":[{"level":"warn","code":"users","text":"3 users"}],"blocked":false,
				"commands":[{"phase":"shutdown","binary":"gfix","command":"gfix -shut full -force 0 -user SYSDBA -password *** /d/a.fdb"}],
				"details":{"n":1},"confirm_token":"tok-1","expires_at":"2026-11-20T10:00:00Z"}`))
		case "/v1/dba/ops/db_shutdown/run":
			_ = json.NewDecoder(r.Body).Decode(&runBody)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"run_id":"r9","run_url":"/v1/dba/runs/r9","run":{"run_id":"r9","op":"db_shutdown","state":"running","step_count":1}}`))
		case "/v1/dba/ops/db_mend/run":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"a check blocks this operation: in use"},"reason":"blocked","checks":[{"level":"block","code":"attached","text":"in use"}]}`))
		case "/v1/dba/ops/nope/check":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"unknown operation"}}`))
		case "/v1/dba/ops/db_copy/check":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"denied","message":"unknown parameter \"x\""}}`))
		case "/v1/dba/ops/plain/run":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"the run has ended"}}`))
		}
	}))
	ctx := context.Background()
	chk, err := cl.DBACheck(ctx, "db_shutdown", DBARequest{Database: "/d/a.fdb"})
	if err != nil {
		t.Fatal(err)
	}
	if chk.Class != DBAClassDisruptive || chk.ConfirmToken != "tok-1" || len(chk.Commands) != 1 || chk.Checks[0].Level != "warn" || string(chk.Details) != `{"n":1}` {
		t.Fatalf("check = %+v", chk)
	}
	st, err := cl.DBARun(ctx, "db_shutdown", DBARequest{Database: "/d/a.fdb", ConfirmToken: chk.ConfirmToken, RequestedBy: "op", Reason: "r", RequestID: "req-1"})
	if err != nil {
		t.Fatal(err)
	}
	if st.RunID != "r9" || st.Run.State != DBAStateRunning || st.Run.Final() {
		t.Fatalf("run = %+v", st)
	}
	if runBody.ConfirmToken != "tok-1" || runBody.RequestedBy != "op" || runBody.RequestID != "req-1" || runBody.Database != "/d/a.fdb" {
		t.Fatalf("run body = %+v", runBody)
	}

	// 409 refusal: typed, with the checks.
	_, err = cl.DBARun(ctx, "db_mend", DBARequest{Database: "/d/a.fdb"})
	var ref *DBARefusal
	if !errors.As(err, &ref) {
		t.Fatalf("want *DBARefusal, got %v", err)
	}
	if ref.Reason != "blocked" || len(ref.Checks) != 1 || ref.Checks[0].Level != "block" {
		t.Fatalf("refusal = %+v", ref)
	}
	if !strings.HasPrefix(err.Error(), "fbagent conflict (409, conflict)") || !strings.Contains(err.Error(), "reason blocked") {
		t.Fatalf("refusal text = %q", err)
	}
	// A plain 409 (no reason, no checks) stays the usual conflict error.
	_, err = cl.DBARun(ctx, "plain", DBARequest{})
	if errors.As(err, &ref) || err == nil || !strings.Contains(err.Error(), "fbagent conflict (409, conflict): the run has ended") {
		t.Fatalf("plain conflict = %v", err)
	}
	if _, err := cl.DBACheck(ctx, "nope", DBARequest{}); err == nil || !strings.Contains(err.Error(), "fbagent not-found") {
		t.Fatalf("unknown op: %v", err)
	}
	if _, err := cl.DBACheck(ctx, "db_copy", DBARequest{}); err == nil || !strings.Contains(err.Error(), "fbagent bad-request (400, denied)") {
		t.Fatalf("bad param: %v", err)
	}
}

func TestDBAOutput(t *testing.T) {
	cl, _ := newTestAgent(t, "t", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/dba/runs/r1/output":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("X-Output-Bytes", "3000000")
			if r.URL.Query().Get("tail") == "2" {
				_, _ = w.Write([]byte("line 9\nline 10\n"))
				return
			}
			w.Header().Set("X-Output-Truncated", "1")
			_, _ = w.Write([]byte("line 1\nline 2\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"this run has no output"}}`))
		}
	}))
	ctx := context.Background()
	out, err := cl.GetDBAOutput(ctx, "r1", 0)
	if err != nil || out.Text != "line 1\nline 2\n" || !out.Truncated || out.Bytes != 3000000 {
		t.Fatalf("view: %v %+v", err, out)
	}
	out, err = cl.GetDBAOutput(ctx, "r1", 2)
	if err != nil || out.Text != "line 9\nline 10\n" || out.Truncated {
		t.Fatalf("tail: %v %+v", err, out)
	}
	if _, err := cl.GetDBAOutput(ctx, "r2", 0); err == nil || !strings.Contains(err.Error(), "this run has no output") {
		t.Fatalf("no output: %v", err)
	}
}
