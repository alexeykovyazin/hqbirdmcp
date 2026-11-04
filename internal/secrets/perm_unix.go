//go:build !windows

package secrets

import (
	"fmt"
	"os"
	"syscall"
)

func isWindows() bool { return false }

// checkPerm refuses a secret file that "other" users can read or write.
func checkPerm(p string, fi os.FileInfo) error {
	if fi.Mode().Perm()&0o007 != 0 {
		return fmt.Errorf("file %s has mode %v: other users can access it (chmod 0600)", p, fi.Mode().Perm())
	}
	return nil
}

// chownLike gives p the owner and group of the folder when run as root, so
// `sudo fbmcpctl secret set --dir` leaves a file the service user can read.
func chownLike(p string, dir os.FileInfo) error {
	if os.Geteuid() != 0 {
		return nil
	}
	st, ok := dir.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return os.Chown(p, int(st.Uid), int(st.Gid))
}
