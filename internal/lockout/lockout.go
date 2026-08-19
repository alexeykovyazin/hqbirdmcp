// Package lockout implements P4.3 guards: never drop/revoke the last admin,
// the operating identity, or the registry read-only user.
package lockout

import (
	"fmt"
	"strings"
)

// DropUser refuses SYSDBA, the admin user, and the registry RO user.
func DropUser(adminUser, roUser, target string) error {
	if target == "" {
		return fmt.Errorf("user required")
	}
	if strings.EqualFold(target, "SYSDBA") || strings.EqualFold(target, adminUser) {
		return fmt.Errorf("refusing to drop the last admin-capable user (%s)", target)
	}
	if strings.EqualFold(target, roUser) {
		return fmt.Errorf("refusing to drop the registry read-only user the server depends on")
	}
	return nil
}

// Revoke refuses stripping the operating identity or the RO user.
func Revoke(adminUser, roUser, grantee string) error {
	if grantee == "" {
		return fmt.Errorf("grantee required")
	}
	if strings.EqualFold(grantee, adminUser) || strings.EqualFold(grantee, "SYSDBA") {
		return fmt.Errorf("refusing to revoke the operating identity's own access")
	}
	if strings.EqualFold(grantee, roUser) {
		return fmt.Errorf("refusing to revoke the registry read-only user")
	}
	return nil
}
