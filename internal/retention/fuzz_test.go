package retention

import (
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/state"
)

func FuzzPlan(f *testing.F) {
	f.Add(0)
	f.Add(7)
	f.Add(-1)
	f.Fuzz(func(t *testing.T, keepDays int) {
		if keepDays > 3650 || keepDays < -10 {
			return
		}
		st, err := state.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		_ = Plan(st, keepDays, time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC))
	})
}
