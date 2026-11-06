package gate

import "strings"

// Out-of-band decision markers. The tray and "fbmcp ctl approve" write one
// file per request id into <state.dir>/approvals or <state.dir>/denials;
// the kernel consumes it. Line 1 is the verdict. Optional "by: " and
// "reason: " lines follow (the tray writes the verdict line only).
const (
	MarkerApproved = "approved-by-operator"
	MarkerDenied   = "denied-by-operator"
)

// maxMarkerField keeps a reason short enough for the audit log.
const maxMarkerField = 300

// MarkerBody formats a marker file.
func MarkerBody(verdict, by, reason string) string {
	var b strings.Builder
	b.WriteString(verdict + "\n")
	if by = markerField(by); by != "" {
		b.WriteString("by: " + by + "\n")
	}
	if reason = markerField(reason); reason != "" {
		b.WriteString("reason: " + reason + "\n")
	}
	return b.String()
}

// ParseMarker reads the optional fields of a marker file.
func ParseMarker(body []byte) (by, reason string) {
	for _, l := range strings.Split(string(body), "\n") {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "by:"):
			by = markerField(strings.TrimPrefix(l, "by:"))
		case strings.HasPrefix(l, "reason:"):
			reason = markerField(strings.TrimPrefix(l, "reason:"))
		}
	}
	return by, reason
}

func markerField(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxMarkerField {
		s = s[:maxMarkerField]
	}
	return s
}
