package main

import (
	"strings"
	"testing"

	"github.com/aleks/fbmcp/internal/facts"
)

func TestFormatConnectedDBs(t *testing.T) {
	got := formatConnectedDBs(&facts.ConnectedDBs{
		Instance:         "fb5",
		AttachmentsCount: 182,
		DatabaseCount:    4,
		Databases: []facts.ConnectedDB{
			{Path: `D:\DATABASE\WMS_CIELO\DADOS.FDB`, DBID: "spike5", MatchStatus: "managed"},
			{Path: `D:\DATABASE\WMS_CIELO\OPLGAR.FDB`, MatchStatus: "ambiguous"},
			{Path: `D:\DATABASE\WMS_CIELO\SINCRONIZADOR2\SINCRONIZA2.FDB`, MatchStatus: "unmanaged"},
		},
	})
	for _, want := range []string{
		"instance: fb5",
		"attachment_count: 182",
		"database_count: 4",
		"path: D:\\DATABASE\\WMS_CIELO\\DADOS.FDB",
		"db: spike5",
		"match_status: ambiguous",
		"match_status: unmanaged",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatted output missing %q:\n%s", want, got)
		}
	}
}

func TestFormatConnectedDBsEmpty(t *testing.T) {
	got := formatConnectedDBs(&facts.ConnectedDBs{Instance: "fb5"})
	if !strings.Contains(got, "(active databases: none)") {
		t.Fatalf("want empty marker, got:\n%s", got)
	}
}
