package errdoc

import "testing"

func TestLookupKnown(t *testing.T) {
	d, ok := Lookup(`read pool ping "spike5": Error loading plugin MySQLEngine
Module C:\HQbird\Firebird50\plugins\MySQLEngine exists but can not be loaded`)
	if !ok || d.Code != "engine-plugin-scan" {
		t.Fatalf("plugin quirk not classified: %+v", d)
	}
	if d, ok := Lookup("DENIED: no open maintenance window for this database"); !ok || d.Code != "precondition-window" {
		t.Fatalf("window deny not classified: %+v", d)
	}
	if _, ok := Lookup("confirmation rejected: this tier cannot be confirmed through the in-band channel (channel policy: tier >= 2 requires out-of-band)"); !ok {
		t.Fatal("channel-policy signal missed")
	}
}

func TestLookupUnknown(t *testing.T) {
	if _, ok := Lookup("something novel"); ok {
		t.Fatal("unknown message must not classify")
	}
}
