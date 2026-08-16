package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	fb "github.com/nakagami/firebirdsql"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, `C:\HQbird\Firebird50\gfix.exe`, "-validate", "-full", "-user", "SYSDBA", `C:\HQbirdData\output\fbmcp-spike\spike_FB5.0.fdb`)
	cmd.Env = append(os.Environ(), "ISC_PASSWORD=masterkey")
	err := cmd.Run()
	fmt.Printf("timeout-kill: err=%v exit=%d\n", err, cmd.ProcessState.ExitCode())

	svc, err := fb.NewServiceManager("localhost:3055", "sysdba", "masterkey", fb.NewServiceManagerOptions())
	if err != nil {
		fmt.Println("svc attach:", err)
		return
	}
	bm, err := fb.NewBackupManager("localhost:3055", "sysdba", "masterkey", fb.NewServiceManagerOptions())
	if err != nil {
		fmt.Println("backup mgr:", err)
		return
	}
	ch := make(chan string, 100)
	err = bm.Backup(`C:/HQbirdData/output/fbmcp-spike/spike_FB5.0.fdb`, `C:/HQbirdData/output/fbmcp-spike/svc_FB5.0.fbk`, fb.GetDefaultBackupOptions(), ch)
	fmt.Printf("driver backup: err=%v\n", err)
	svc.Close()
}
