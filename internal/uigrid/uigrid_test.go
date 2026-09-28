package uigrid

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testPayload(rows int) *Payload {
	cols := []Col{{Name: "ID", Type: "integer"}, {Name: "NAME", Type: "string"}}
	r := make([][]any, rows)
	for i := range r {
		r[i] = []any{int64(i + 1), "row-" + strings.Repeat("x", 10)}
	}
	return Gridify("db: SELECT 1", cols, r, Options{})
}

func clockAt(start time.Time) func() time.Time {
	t := start
	return func() time.Time { return t }
}

func TestRegistryPutGet(t *testing.T) {
	now := time.Now()
	r := New(RegistryOpts{Enabled: true, Now: clockAt(now)})
	id, ok := r.Put("local", "t", testPayload(3))
	if !ok || id == "" {
		t.Fatalf("Put disabled or empty id: %v %q", ok, id)
	}
	if !strings.HasPrefix(URI(id), URIPrefix) {
		t.Fatalf("URI %q not under %s", URI(id), URIPrefix)
	}
	data, err := r.Get("local", id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var p Payload
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("payload json: %v", err)
	}
	if p.Schema != SchemaVersion || p.RowCount != 3 || len(p.Columns) != 2 {
		t.Fatalf("unexpected payload %+v", p)
	}
}

func TestRegistryExpiry(t *testing.T) {
	now := time.Now()
	r := New(RegistryOpts{Enabled: true, TTL: time.Minute, Now: clockAt(now)})
	id, _ := r.Put("local", "t", testPayload(1))
	if _, err := r.Get("local", id); err != nil {
		t.Fatalf("fresh Get: %v", err)
	}
	// advance the fake clock past the TTL
	r.opts.Now = clockAt(now.Add(2 * time.Minute))
	if _, err := r.Get("local", id); err != ErrExpired {
		t.Fatalf("expired Get err = %v, want ErrExpired", err)
	}
	if !strings.HasPrefix(ErrExpired.Error(), "result-expired") {
		t.Fatalf("error must lead with the stable code: %q", ErrExpired)
	}
}

func TestRegistryForbidden(t *testing.T) {
	r := New(RegistryOpts{Enabled: true})
	id, _ := r.Put("alice", "t", testPayload(1))
	if _, err := r.Get("bob", id); err != ErrForbidden {
		t.Fatalf("cross-identity Get err = %v, want ErrForbidden", err)
	}
}

func TestRegistryMaxLiveEviction(t *testing.T) {
	r := New(RegistryOpts{Enabled: true, MaxLive: 2})
	a1, _ := r.Put("local", "t", testPayload(1))
	a2, _ := r.Put("local", "t", testPayload(1))
	a3, _ := r.Put("local", "t", testPayload(1))
	if _, err := r.Get("local", a1); err != ErrExpired {
		t.Fatalf("oldest should be evicted, err = %v", err)
	}
	for _, id := range []string{a2, a3} {
		if _, err := r.Get("local", id); err != nil {
			t.Fatalf("survivor %s: %v", id, err)
		}
	}
}

func TestRegistryDisabled(t *testing.T) {
	r := New(RegistryOpts{Enabled: false})
	if _, ok := r.Put("local", "t", testPayload(1)); ok {
		t.Fatal("disabled registry must not store")
	}
	if r.Enabled() {
		t.Fatal("Enabled() true on disabled registry")
	}
	var nilRegistry *Registry
	if nilRegistry.Enabled() {
		t.Fatal("nil registry must report disabled")
	}
}

func TestRegistrySweepFreesIdentitySlots(t *testing.T) {
	now := time.Now()
	r := New(RegistryOpts{Enabled: true, TTL: time.Minute, MaxLive: 2, Now: clockAt(now)})
	r.Put("local", "t", testPayload(1))
	r.Put("local", "t", testPayload(1))
	r.opts.Now = clockAt(now.Add(2 * time.Minute)) // both expire
	id, _ := r.Put("local", "t", testPayload(1))   // sweep runs inside Put
	if _, err := r.Get("local", id); err != nil {
		t.Fatalf("post-sweep Put/Get: %v", err)
	}
	if len(r.order["local"]) != 1 {
		t.Fatalf("sweep must clear ordering slots, got %d", len(r.order["local"]))
	}
}

func TestGridifyCellClip(t *testing.T) {
	long := strings.Repeat("y", 600)
	p := Gridify("t", []Col{{Name: "S", Type: "string"}}, [][]any{{long, nil, int64(7), true}}, Options{})
	cell := p.Rows[0][0].(string)
	if !strings.HasPrefix(cell, strings.Repeat("y", CellClip)) || !strings.HasSuffix(cell, clipMarker) {
		t.Fatalf("cell not clipped: len=%d", len(cell))
	}
	if len(cell) > CellClip+len(clipMarker) {
		t.Fatalf("clipped cell too long: %d", len(cell))
	}
	if p.Rows[0][1] != nil || p.Rows[0][2] != int64(7) || p.Rows[0][3] != true {
		t.Fatalf("non-string cells must pass through: %+v", p.Rows[0])
	}
}

func TestGridifyNilForZeroColumns(t *testing.T) {
	if p := Gridify("t", nil, nil, Options{}); p != nil {
		t.Fatal("zero-column result must yield nil payload (no ui reference)")
	}
}

func TestPutByteCapDropsRows(t *testing.T) {
	r := New(RegistryOpts{Enabled: true, MaxBytes: 512})
	// each row ~40 bytes of 'x' → 512-byte cap must drop many
	p := testPayload(100)
	for i := range p.Rows {
		p.Rows[i][1] = strings.Repeat("x", 40)
	}
	id, ok := r.Put("local", "t", p)
	if !ok {
		t.Fatal("over-cap payload must still serve (clipped), not fail")
	}
	data, err := r.Get("local", id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(data) > 512 {
		t.Fatalf("payload %d bytes over cap", len(data))
	}
	var got Payload
	_ = json.Unmarshal(data, &got)
	if got.ClippedRows == 0 || got.ClippedRows != 100-got.RowCount {
		t.Fatalf("clipped accounting wrong: row_count=%d clipped=%d", got.RowCount, got.ClippedRows)
	}
}

func TestMapType(t *testing.T) {
	cases2 := []struct {
		name     string
		db       string
		scale    int64
		hasScale bool
		want     string
	}{
		{"short", "SHORT", 0, false, "integer"},
		{"long", "LONG", 0, false, "integer"},
		{"bigint", "INT64", 0, true, "integer"},
		{"numeric18", "INT64", -4, true, "decimal"},
		{"hugeint", "INT128", 0, true, "integer"},
		{"decfloat34", "DECFLOAT(34)", 0, false, "decimal"},
		{"decfixed", "DECFIXED", 0, false, "decimal"},
		{"float", "FLOAT", 0, false, "float"},
		{"double", "DOUBLE", 0, false, "float"},
		{"varchar", "VARYING", 0, false, "string"},
		{"char", "TEXT", 0, false, "string"},
		{"date", "DATE", 0, false, "date"},
		{"time", "TIME", 0, false, "time"},
		{"ts", "TIMESTAMP", 0, false, "timestamp"},
		{"tstz", "TIMESTAMP WITH TIMEZONE", 0, false, "timestamp"},
		{"bool", "BOOLEAN", 0, false, "boolean"},
		{"blob", "BLOB", 0, false, "blob"},
		{"array", "ARRAY", 0, false, "unknown"},
		{"empty", "", 0, false, "unknown"},
		{"lower", "int64", -2, true, "decimal"},
	}
	for _, c := range cases2 {
		if got := MapType(c.db, c.scale, c.hasScale); got != c.want {
			t.Errorf("MapType(%q,%d,%v) = %q, want %q", c.db, c.scale, c.hasScale, got, c.want)
		}
	}
}

func TestRenderSafety(t *testing.T) {
	// payload carrying breakout + external-URL attempts must come out inert
	p := Gridify("t", []Col{{Name: "S", Type: "string"}},
		[][]any{{"</script><script>alert(1)</script>", "http://evil.example/x", "&amp;"}}, Options{})
	data, _ := json.Marshal(p)
	html := Render(data)
	if strings.Contains(html, "</script><script>alert(1)") {
		t.Fatal("payload breakout not escaped")
	}
	if !strings.Contains(html, `"S"`) {
		t.Fatal("payload not present")
	}
	// payload escaped via \u003c so the JSON stays valid for the renderer
	if !strings.Contains(html, `\u003c/script\u003e`) {
		t.Fatal("expected \\u003c escaping of '<'")
	}
}

func TestBundleCSP(t *testing.T) {
	// the bundle must be self-contained: CSP denies everything but inline,
	// and no external reference (src/href/url()) may exist anywhere
	if !strings.Contains(bundleHTML, `http-equiv="Content-Security-Policy"`) {
		t.Fatal("bundle lacks CSP meta")
	}
	for _, bad := range []string{"http://", "https://", "//cdn", "eval(", "new Function", "fetch(", "XMLHttpRequest", "import("} {
		if strings.Contains(bundleHTML, bad) {
			t.Errorf("bundle contains forbidden sequence %q", bad)
		}
	}
	if _, err := json.Marshal(testPayload(1)); err != nil {
		t.Fatalf("fixture payload: %v", err)
	}
}

func TestRenderTemplateIsDataOnly(t *testing.T) {
	// the data slot must be the single template action
	if strings.Count(bundleHTML, "{{") != 1 {
		t.Fatalf("bundle must contain exactly one template action, got %d", strings.Count(bundleHTML, "{{"))
	}
}
