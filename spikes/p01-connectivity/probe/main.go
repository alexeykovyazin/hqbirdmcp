// P0.2 T3 probe: Services API via driver (version + backup + gstat-style stats).
package main

import (
	"fmt"

	fb "github.com/nakagami/firebirdsql"
)

func main() {
	for _, p := range []struct{ name, addr string }{
		{"FB3.0", "localhost:3053"}, {"FB4.0", "localhost:3054"}, {"FB5.0", "localhost:3055"},
	} {
		svc, err := fb.NewServiceManager(p.addr, "sysdba", "masterkey", fb.NewServiceManagerOptions())
		if err != nil {
			fmt.Printf("[%s] svc attach: FAIL %v\n", p.name, err)
			continue
		}
		v, err := svc.GetServerVersionString()
		if err != nil {
			fmt.Printf("[%s] version: FAIL %v\n", p.name, err)
		} else {
			fmt.Printf("[%s] version: OK %q\n", p.name, v)
		}
		svc.Close()
	}
}
