//go:build windows

package main

import (
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// serviceSID returns the SID of the virtual account "NT SERVICE\<name>".
// It is derived from the name alone (the same value as `sc showsid`), so
// the key can be granted before the service exists.
func serviceSID(name string) string {
	u := utf16.Encode([]rune(strings.ToUpper(name)))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	h := sha1.Sum(b)
	parts := []string{"S-1-5-80"}
	for i := 0; i < 5; i++ {
		parts = append(parts, fmt.Sprint(binary.LittleEndian.Uint32(h[4*i:])))
	}
	return strings.Join(parts, "-")
}

// accountSID returns the SID string of an account. A virtual service
// account is computed (it exists only after the service is created); other
// names are looked up.
func accountSID(account string) (string, error) {
	if svc, ok := strings.CutPrefix(account, `NT SERVICE\`); ok {
		return serviceSID(svc), nil
	}
	if sid, ok := strings.CutPrefix(account, "*"); ok && strings.HasPrefix(sid, "S-") {
		return sid, nil
	}
	sid, _, _, err := windows.LookupSID("", account)
	if err != nil {
		return "", fmt.Errorf("account %s: %w", account, err)
	}
	return sid.String(), nil
}

// grantKeyRead sets the key ACL (F9): Administrators and SYSTEM full,
// the service account read, nothing inherited. It writes the DACL through
// the API, not icacls: icacls looks the SID up and fails with error 1332
// for a service that does not exist yet (phase 12 test W).
func grantKeyRead(path, account string) error {
	sid, err := accountSID(account)
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;BA)(A;;FA;;;SY)(A;;FR;;;" + sid + ")")
	if err != nil {
		return fmt.Errorf("key ACL: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("key ACL: %w", err)
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

// keepOwner: Windows files take the folder ACL; grantKeyRead sets the key.
func keepOwner(tmp, target string) error { return nil }
