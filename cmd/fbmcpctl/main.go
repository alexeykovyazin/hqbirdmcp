// fbmcpctl — the operator CLI. A thin wrapper: the commands live in
// internal/ctl and also run as "fbmcp ctl ..." (ADR-038).
package main

import (
	"os"

	"github.com/aleks/fbmcp/internal/ctl"
)

// version is set at build time, as in cmd/fbmcp.
var version = "dev"

func main() {
	ctl.Version = version
	os.Exit(ctl.Main(os.Args[1:]))
}
