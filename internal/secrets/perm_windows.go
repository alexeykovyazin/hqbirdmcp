//go:build windows

package secrets

import "os"

func isWindows() bool { return true }

// checkPerm is a no-op on Windows: the file inherits the folder ACL, which
// install-remote.ps1 limits to the service account and Administrators.
func checkPerm(string, os.FileInfo) error { return nil }

// chownLike is a no-op on Windows (the inherited folder ACL applies).
func chownLike(string, os.FileInfo) error { return nil }
