//go:build !windows

package ctl

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// grantKeyRead: mode 0600, owner = the service user (F9).
func grantKeyRead(path, account string) error {
	u, err := user.Lookup(account)
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("chown %s (run as root): %w", path, err)
	}
	return os.Chmod(path, 0o600)
}

// keepOwner gives the temp file the owner of the file it replaces, so a
// --renew run by root keeps the key and cert readable by the service user.
func keepOwner(tmp, target string) error {
	fi, err := os.Stat(target)
	if err != nil {
		return nil
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || os.Geteuid() != 0 {
		return nil
	}
	return os.Chown(tmp, int(st.Uid), int(st.Gid))
}
