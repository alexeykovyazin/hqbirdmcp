package schedule

import (
	"testing"
	"time"
)

func FuzzMatches(f *testing.F) {
	f.Add("0 3 * * *")
	f.Add("* * * * *")
	f.Add("*/5 0-12 1,15 * 0")
	f.Add("bad")
	seed := time.Date(2026, 8, 17, 3, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, expr string) {
		if len(expr) > 64 {
			return
		}
		_, _ = Matches(expr, seed)
	})
}
