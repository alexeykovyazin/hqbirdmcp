package lwmonitoring

import (
	"context"
	"testing"

	"github.com/aleks/fbmcp/internal/config"
)

func TestTargetFor(t *testing.T) {
	cases := map[string]string{
		"localhost:3055": "localhost/3055:service_mgr",
		"127.0.0.1:3050": "127.0.0.1/3050:service_mgr",
		"dbserver:3055":  "dbserver/3055:service_mgr",
		"dbserver":       "dbserver:service_mgr",
		"":               "localhost:service_mgr",
	}
	for addr, want := range cases {
		if got := targetFor(addr); got != want {
			t.Errorf("targetFor(%q) = %q, want %q", addr, got, want)
		}
	}
}

func TestQueryLevelBounds(t *testing.T) {
	inst := config.FBInstance{Addr: "localhost:3055", BinDir: "C:/HQbird/Firebird50"}
	for _, level := range []int{0, 5, -1} {
		if _, err := Query(context.Background(), inst, "SYSDBA", "masterkey", level, ""); err == nil {
			t.Errorf("level %d: expected out-of-range error, got nil", level)
		}
	}
}
