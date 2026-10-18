package agentapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestAgent spins an httptest server standing in for the fbagent Local
// API and returns a client pointed at it (plus the server for mutation).
func newTestAgent(t *testing.T, token string, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cl, err := New(srv.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	return cl, srv
}

func TestBearerTokenSent(t *testing.T) {
	var gotAuth string
	cl, _ := newTestAgent(t, "sekrit", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	raw, err := cl.GetDatabases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sekrit" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if string(raw) != `{"ok":true}` {
		t.Fatalf("raw = %s", raw)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		wantMarker string
	}{
		{"auth", `{"error":{"code":"denied","message":"unauthorized"}}`, 401, "fbagent auth (401, denied): unauthorized"},
		{"notfound", `{"error":{"code":"not_found","message":"no such job"}}`, 404, "fbagent not-found (404, not_found): no such job"},
		{"badrequest", `{"error":{"code":"denied","message":"invalid glob"}}`, 400, "fbagent bad-request (400, denied): invalid glob"},
		{"methodnotallowed", `{"error":{"code":"denied","message":"method not allowed"}}`, 405, "fbagent bad-request (405, denied): method not allowed"},
		{"unavailable", `{"error":{"code":"unavailable","message":"data_path not configured"}}`, 503, "fbagent unavailable (503, unavailable): data_path not configured"},
		{"serviceerror", `{"error":{"code":"service_error","message":"read failed"}}`, 500, "fbagent service-error (500, service_error): read failed"},
		{"toolarge", `{"error":{"code":"payload_too_large","message":"over cap"}}`, 413, "fbagent payload-too-large (413, payload_too_large): over cap"},
		{"statusonly", `nonsense`, 503, "fbagent unavailable (503, ): nonsense"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cl, _ := newTestAgent(t, "t", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			_, err := cl.GetDatabases(context.Background())
			if err == nil {
				t.Fatal("want error")
			}
			if err.Error() != tc.wantMarker {
				t.Fatalf("err = %q\nwant %q", err.Error(), tc.wantMarker)
			}
		})
	}
}

func TestTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listening
	cl, err := New(url, "t")
	if err != nil {
		t.Fatal(err)
	}
	_, err = cl.GetDatabases(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "fbagent transport") {
		t.Fatalf("err = %v", err)
	}
}

func TestEngineOpAndPoll(t *testing.T) {
	polls := 0
	cl, _ := newTestAgent(t, "t", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/instances/fb1/restart":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["requested_by"] != "cc" {
				t.Errorf("requested_by = %v", body["requested_by"])
			}
			_, _ = w.Write([]byte(`{"operation_id":"op-1"}`))
		case r.URL.Path == "/v1/operations/op-1":
			polls++
			if polls < 2 {
				_, _ = w.Write([]byte(`{"operation_id":"op-1","kind":"restart","state":"queued"}`))
				return
			}
			_, _ = w.Write([]byte(`{"operation_id":"op-1","kind":"restart","state":"succeeded","finished_at":"2026-09-13T10:00:12Z"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	resp, err := cl.EngineOp(context.Background(), "fb1", "restart", EngineOpRequest{RequestedBy: "cc"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.OperationID != "op-1" {
		t.Fatalf("operation_id = %q", resp.OperationID)
	}
	op, err := cl.PollOperation(context.Background(), "op-1", 5*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != "succeeded" {
		t.Fatalf("state = %q", op.State)
	}
	if polls < 2 {
		t.Fatalf("polls = %d, want the queued state observed", polls)
	}
}

func TestPollFailedOpReturnsDocNotError(t *testing.T) {
	cl, _ := newTestAgent(t, "t", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"operation_id":"op-2","kind":"restart","state":"failed","error":{"code":"timeout","message":"firebird did not accept connections"}}`))
	}))
	op, err := cl.PollOperation(context.Background(), "op-2", time.Millisecond, nil)
	if err != nil {
		t.Fatalf("terminal failed op must not be a transport error: %v", err)
	}
	if op.State != "failed" || op.Error == nil || op.Error.Code != "timeout" {
		t.Fatalf("op = %+v", op)
	}
}

func TestPollDeadline(t *testing.T) {
	cl, _ := newTestAgent(t, "t", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"operation_id":"op-3","kind":"restart","state":"in_progress"}`))
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := cl.PollOperation(ctx, "op-3", 25*time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "fbagent op-timeout") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunJobAndWarnings(t *testing.T) {
	cl, _ := newTestAgent(t, "t", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/jobs/sweep/run" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var req JobRunRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.JobID != "nightly" {
			t.Errorf("job_id = %q", req.JobID)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"operation_id":"op-9","job":"sweep","status_url":"/v1/jobs/sweep/status","warnings":["sync skipped; enqueued forced run instead"]}`))
	}))
	resp, err := cl.RunJob(context.Background(), "sweep", JobRunRequest{JobID: "nightly"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Warnings) != 1 || resp.StatusURL == "" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestArtifactGuards(t *testing.T) {
	for _, bad := range []string{"", "a/b", `a\b`, "..", "a.."} {
		if err := ValidateScope(bad); err == nil {
			t.Errorf("ValidateScope(%q) = nil", bad)
		}
	}
	if err := ValidateScope("server"); err != nil {
		t.Errorf("server scope refused: %v", err)
	}
	if err := ValidateScope("C__db_mydb"); err != nil {
		t.Errorf("dbKey refused: %v", err)
	}
	for _, bad := range []string{"", "a/../b", `a\b`, "./a", "a//b"} {
		if err := ValidateArtifactPath(bad); err == nil {
			t.Errorf("ValidateArtifactPath(%q) = nil", bad)
		}
	}
	qCases := []struct {
		scope, path string
		q           ArtifactQuery
		ok          bool
	}{
		{"C__db", "metrics/latest", ArtifactQuery{Tail: 10}, true},
		{"server", "backups/status", ArtifactQuery{Tail: 10}, false},
		{"C__db", "gstat-full/latest", ArtifactQuery{Tail: 3}, true},
		{"server", "lwm/summary", ArtifactQuery{Date: "2026-09-13"}, true},
		{"server", "lwm/raw", ArtifactQuery{At: "2026-09-13T10:00:00Z"}, true},
		{"server", "lwm/summary", ArtifactQuery{Date: "2026-09-12"}, true},
		{"server", "backups/status", ArtifactQuery{Date: "2026-09-13"}, false},
		{"server", "config/snapshots", ArtifactQuery{Limit: 5}, true},
		{"server", "traces/processed", ArtifactQuery{Limit: 300}, true}, // range is agent-side
		{"C__db", "metrics/latest", ArtifactQuery{Limit: 5}, false},
		{"server", "backups/status", ArtifactQuery{At: "2026-09-13T10:00:00Z"}, false},
	}
	for i, tc := range qCases {
		err := ValidateArtifactQuery(tc.scope, tc.path, tc.q)
		if (err == nil) != tc.ok {
			t.Errorf("case %d (%s %s %+v): err = %v, want ok=%v", i, tc.scope, tc.path, tc.q, err, tc.ok)
		}
	}
}

func TestGetArtifactQueryParams(t *testing.T) {
	var gotPath, gotQuery string
	cl, _ := newTestAgent(t, "t", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		_, _ = w.Write([]byte(`{"files":[],"count":0}`))
	}))
	if _, err := cl.GetArtifact(context.Background(), "C__db", "metrics/latest", ArtifactQuery{Tail: 7}); err != nil {
		t.Fatal(err)
	}
	if gotQuery != "tail=7" {
		t.Fatalf("query = %q", gotQuery)
	}
	// "server" is a required path segment on the listing route (probed 404
	// without it — phase9L).
	if _, err := cl.GetArtifactList(context.Background(), "server", "*"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/artifacts/server" {
		t.Fatalf("listing path = %q", gotPath)
	}
	if _, err := cl.GetArtifactList(context.Background(), "C__db", "*"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/artifacts/C__db" {
		t.Fatalf("dbKey listing path = %q", gotPath)
	}
	if _, err := cl.GetArtifactList(context.Background(), "", "*"); err == nil {
		t.Fatal("empty scope accepted")
	}
}

func TestRestartOpIDExtraction(t *testing.T) {
	var op Operation
	if err := json.Unmarshal([]byte(`{"operation_id":"u","state":"succeeded","result":{"restart_operation_id":"re-1"}}`), &op); err != nil {
		t.Fatal(err)
	}
	if op.RestartOpID() != "re-1" {
		t.Fatalf("RestartOpID = %q", op.RestartOpID())
	}
	var direct Operation
	if err := json.Unmarshal([]byte(`{"operation_id":"u","state":"succeeded","restart_operation_id":"re-2"}`), &direct); err != nil {
		t.Fatal(err)
	}
	if direct.RestartOpID() != "re-2" {
		t.Fatalf("RestartOpID = %q", direct.RestartOpID())
	}
}

// --- 2.60+ run-now / license / product-update additions (API 2.67) ---

func TestConflictMapping(t *testing.T) {
	cl, _ := newTestAgent(t, "t", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"run rn-1 already started"}}`))
	}))
	_, err := cl.CancelRun(context.Background(), "rn-1")
	if err == nil || !strings.HasPrefix(err.Error(), "fbagent conflict (409, conflict): run rn-1 already started") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunNowEndpoints(t *testing.T) {
	var gotQuery, gotMethod string
	cl, _ := newTestAgent(t, "t", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotQuery = r.Method, r.URL.RawQuery
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/rn-1":
			_, _ = w.Write([]byte(`{"run_id":"rn-1","state":"waiting","state_reason":"db lock"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs":
			_, _ = w.Write([]byte(`{"runs":[{"run_id":"rn-2","state":"queued"}]}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/runs/rn-2":
			_, _ = w.Write([]byte(`{"run_id":"rn-2","state":"cancelled"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	raw, err := cl.GetRun(context.Background(), "rn-1")
	if err != nil || !strings.Contains(string(raw), `"waiting"`) {
		t.Fatalf("GetRun: %v %s", err, raw)
	}
	if _, err := cl.GetRuns(context.Background(), "vbackup", "job-7", true, 5); err != nil {
		t.Fatal(err)
	}
	if gotQuery != "active=1&job=vbackup&job_id=job-7&limit=5" {
		t.Fatalf("list query = %q", gotQuery)
	}
	raw, err = cl.CancelRun(context.Background(), "rn-2")
	if err != nil || gotMethod != http.MethodDelete || !strings.Contains(string(raw), "cancelled") {
		t.Fatalf("CancelRun: %v %s %s", err, gotMethod, raw)
	}
}

func TestRunJobRunNowResponse(t *testing.T) {
	cl, _ := newTestAgent(t, "t", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req JobRunRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.RequestID != "req-12345678" || req.ExpiresAt != "2026-10-18T12:00:00Z" {
			t.Errorf("run-now body fields = %q %q", req.RequestID, req.ExpiresAt)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"operation_id":"op-1","job":"vbackup","status_url":"/v1/jobs/vbackup/status","warnings":[],"run_id":"rn-3f","run_url":"/v1/runs/rn-3f","run":{"state":"queued"}}`))
	}))
	resp, err := cl.RunJob(context.Background(), "vbackup", JobRunRequest{RequestID: "req-12345678", ExpiresAt: "2026-10-18T12:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.RunID != "rn-3f" || resp.RunURL != "/v1/runs/rn-3f" || len(resp.Run) == 0 {
		t.Fatalf("run-now resp = %+v", resp)
	}
}

func TestLicenseEndpoints(t *testing.T) {
	var gotBody string
	cl, _ := newTestAgent(t, "t", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody = ""
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/license":
			_, _ = w.Write([]byte(`{"entitlements":{"state":"licensed"},"status":{"enforce":false}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/v1/license":
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			_, _ = w.Write([]byte(`{"entitlements":{"state":"licensed","license_id":"L-1"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/license/request":
			_, _ = w.Write([]byte(`{"schema":1,"install_id":"inst","id_source":"machine-guid"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	raw, err := cl.GetLicense(context.Background())
	if err != nil || !strings.Contains(string(raw), "licensed") {
		t.Fatalf("GetLicense: %v %s", err, raw)
	}
	doc := json.RawMessage(`{"license":"doc"}`)
	if _, err := cl.SetLicense(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	if gotBody != `{"license":"doc"}` {
		t.Fatalf("PUT body = %q", gotBody)
	}
	if _, err := cl.GetLicenseRequest(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestProductUpdateRoutes(t *testing.T) {
	var gotPath string
	cl, _ := newTestAgent(t, "t", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/instances/fb1/products/hqmonitor/update/status":
			_, _ = w.Write([]byte(`{"product":"hqmonitor","action":"up_to_date"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/instances/fb1/products/hqclusternode/update/apply":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"operation_id":"p9-7","job":"hqclusternode_update"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	if _, err := cl.GetProductUpdateStatus(context.Background(), "fb1", "hqmonitor"); err != nil {
		t.Fatal(err)
	}
	resp, err := cl.ApplyProductUpdate(context.Background(), "fb1", "hqclusternode", "tester", "test")
	if err != nil || resp.OperationID != "p9-7" {
		t.Fatalf("ApplyProductUpdate: %v %+v", err, resp)
	}
	if gotPath != "/v1/instances/fb1/products/hqclusternode/update/apply" {
		t.Fatalf("apply path = %q", gotPath)
	}
}
