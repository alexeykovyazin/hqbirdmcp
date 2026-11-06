package gate

import (
	"strings"
	"testing"
)

func TestMarkerRoundTrip(t *testing.T) {
	body := MarkerBody(MarkerDenied, `HOST\ops`, "wait for\r\nthe backup")
	if !strings.HasPrefix(body, MarkerDenied+"\n") {
		t.Fatalf("verdict line: %q", body)
	}
	by, reason := ParseMarker([]byte(body))
	if by != `HOST\ops` || reason != "wait for the backup" {
		t.Fatalf("by=%q reason=%q", by, reason)
	}
	// The tray writes the verdict line only.
	if by, reason := ParseMarker([]byte(MarkerApproved + "\n")); by != "" || reason != "" {
		t.Fatalf("tray marker: by=%q reason=%q", by, reason)
	}
	if _, reason := ParseMarker([]byte(MarkerBody(MarkerDenied, "", strings.Repeat("x", 1000)))); len(reason) != maxMarkerField {
		t.Fatalf("reason not capped: %d", len(reason))
	}
}
