package lockout

import "testing"

func TestDropUserGuards(t *testing.T) {
	if err := DropUser("SYSDBA", "RO", "SYSDBA"); err == nil {
		t.Fatal("SYSDBA")
	}
	if err := DropUser("admin", "ro", "ADMIN"); err == nil {
		t.Fatal("admin")
	}
	if err := DropUser("admin", "reader", "READER"); err == nil {
		t.Fatal("ro")
	}
	if err := DropUser("admin", "reader", "alice"); err != nil {
		t.Fatal(err)
	}
}

func TestRevokeGuards(t *testing.T) {
	if err := Revoke("admin", "ro", "admin"); err == nil {
		t.Fatal("self")
	}
	if err := Revoke("admin", "ro", "SYSDBA"); err == nil {
		t.Fatal("sysdba")
	}
	if err := Revoke("admin", "ro", "ro"); err == nil {
		t.Fatal("ro")
	}
	if err := Revoke("admin", "ro", "bob"); err != nil {
		t.Fatal(err)
	}
}
