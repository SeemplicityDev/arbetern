package grafana

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var relativeTimeRe = regexp.MustCompile(`^now(?:([+-])(\d+)([smhdwMy]))?(?:/([smhdwMy]))?$`)

// ParseTime accepts now-6h style times (roundUp ends a /unit at the unit's end), RFC 3339, dates and unix s or ms.
func ParseTime(s string, now time.Time, roundUp bool) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if m := relativeTimeRe.FindStringSubmatch(s); m != nil {
		t := now.UTC()
		if m[1] != "" {
			n, _ := strconv.Atoi(m[2])
			if m[1] == "-" {
				n = -n
			}
			t = shift(t, n, m[3])
		}
		if m[4] != "" {
			t = roundTo(t, m[4], roundUp)
		}
		return t, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n > 1e12 {
			return time.UnixMilli(n).UTC(), nil
		}
		return time.Unix(n, 0).UTC(), nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised time %q: use now-6h style, RFC 3339 or unix milliseconds", s)
}

// TimeRange resolves a start/end pair, defaulting to the last hour.
func TimeRange(start, end string, now time.Time) (time.Time, time.Time, error) {
	from, err := ParseTime(start, now, false)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	to, err := ParseTime(end, now, true)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if to.IsZero() {
		to = now.UTC()
	}
	if from.IsZero() {
		from = to.Add(-time.Hour)
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, fmt.Errorf("start %s is not before end %s", from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	return from, to, nil
}

func shift(t time.Time, n int, unit string) time.Time {
	switch unit {
	case "s":
		return t.Add(time.Duration(n) * time.Second)
	case "m":
		return t.Add(time.Duration(n) * time.Minute)
	case "h":
		return t.Add(time.Duration(n) * time.Hour)
	case "d":
		return t.AddDate(0, 0, n)
	case "w":
		return t.AddDate(0, 0, 7*n)
	case "M":
		return t.AddDate(0, n, 0)
	case "y":
		return t.AddDate(n, 0, 0)
	}
	return t
}

func roundTo(t time.Time, unit string, up bool) time.Time {
	var start time.Time
	switch unit {
	case "s":
		start = t.Truncate(time.Second)
	case "m":
		start = t.Truncate(time.Minute)
	case "h":
		start = t.Truncate(time.Hour)
	case "d":
		start = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	case "w":
		d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		start = d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
	case "M":
		start = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	case "y":
		start = time.Date(t.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	default:
		return t
	}
	if !up {
		return start
	}
	return shift(start, 1, unit).Add(-time.Millisecond)
}
