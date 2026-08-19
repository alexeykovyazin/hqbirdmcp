package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type cronSpec struct {
	min, hour, dom, mon, dow field
}

type field struct {
	any bool
	set map[int]bool
}

func parseCron(expr string) (cronSpec, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return cronSpec{}, fmt.Errorf("cron must have 5 fields (min hour dom month dow), got %q", expr)
	}
	min, err := parseField(parts[0], 0, 59)
	if err != nil {
		return cronSpec{}, fmt.Errorf("minute: %w", err)
	}
	hour, err := parseField(parts[1], 0, 23)
	if err != nil {
		return cronSpec{}, fmt.Errorf("hour: %w", err)
	}
	dom, err := parseField(parts[2], 1, 31)
	if err != nil {
		return cronSpec{}, fmt.Errorf("dom: %w", err)
	}
	mon, err := parseField(parts[3], 1, 12)
	if err != nil {
		return cronSpec{}, fmt.Errorf("month: %w", err)
	}
	dow, err := parseField(parts[4], 0, 6)
	if err != nil {
		return cronSpec{}, fmt.Errorf("dow: %w", err)
	}
	return cronSpec{min, hour, dom, mon, dow}, nil
}

func parseField(s string, lo, hi int) (field, error) {
	if s == "*" {
		return field{any: true}, nil
	}
	f := field{set: map[int]bool{}}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "*/") {
			step, err := strconv.Atoi(part[2:])
			if err != nil || step < 1 {
				return field{}, fmt.Errorf("bad step %q", part)
			}
			for v := lo; v <= hi; v += step {
				f.set[v] = true
			}
			continue
		}
		if a, b, ok := strings.Cut(part, "-"); ok {
			from, err1 := strconv.Atoi(a)
			to, err2 := strconv.Atoi(b)
			if err1 != nil || err2 != nil || from < lo || to > hi || from > to {
				return field{}, fmt.Errorf("bad range %q", part)
			}
			for v := from; v <= to; v++ {
				f.set[v] = true
			}
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < lo || n > hi {
			return field{}, fmt.Errorf("bad value %q", part)
		}
		f.set[n] = true
	}
	return f, nil
}

func (f field) hit(v int) bool { return f.any || f.set[v] }

// Matches reports whether t (already in the schedule timezone) hits expr.
func Matches(expr string, t time.Time) (bool, error) {
	c, err := parseCron(expr)
	if err != nil {
		return false, err
	}
	dow := int(t.Weekday()) // Sunday = 0
	return c.min.hit(t.Minute()) && c.hour.hit(t.Hour()) && c.dom.hit(t.Day()) && c.mon.hit(int(t.Month())) && c.dow.hit(dow), nil
}

// PreviousMatch returns the most recent matching minute strictly before t.
func PreviousMatch(expr string, loc *time.Location, t time.Time) (time.Time, bool) {
	cur := t.In(loc).Truncate(time.Minute).Add(-time.Minute)
	limit := cur.Add(-8 * 24 * time.Hour)
	for !cur.Before(limit) {
		ok, err := Matches(expr, cur)
		if err == nil && ok {
			return cur, true
		}
		cur = cur.Add(-time.Minute)
	}
	return time.Time{}, false
}
