package main

import (
	"encoding/json"
	"io"
	"os"
	"testing"
)

// "fbmcp version" prints one JSON line for fbagent VersionFromCLI and never
// starts the kernel (docs/hqbird.md).
func TestVersionCommand(t *testing.T) {
	oldArgs, oldOut, oldVer := os.Args, os.Stdout, version
	defer func() { os.Args, os.Stdout, version = oldArgs, oldOut, oldVer }()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Args, os.Stdout, version = []string{"fbmcp", "version"}, w, "2027.2.20"
	main()
	w.Close()
	b, _ := io.ReadAll(r)
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &v); err != nil || v.Version != "2027.2.20" {
		t.Fatalf("output %q: %v", b, err)
	}
}
