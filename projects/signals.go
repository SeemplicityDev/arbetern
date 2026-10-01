package projects

import (
	"regexp"
	"slices"
	"time"

	"github.com/justmike1/arbetern/datadog"
	"github.com/justmike1/arbetern/internal/redact"
	"github.com/justmike1/arbetern/internal/text"
)

func scanWindow(cursor string, lookback time.Duration, now time.Time) (since, until time.Time) {
	until = now.Add(-ingestionLag)
	since, err := time.Parse(time.RFC3339Nano, cursor)
	if err != nil || since.IsZero() {
		since = now.Add(-lookback)
	}
	if floor := now.Add(-maxLookback); since.Before(floor) {
		since = floor
	}
	return since, until
}

// catchesUp reports whether resuming at next reaches until within maxScanCatchUp if every interval reads as much log time as since..next.
func catchesUp(since, next, until time.Time, every time.Duration) bool {
	gain := next.Sub(since) - every
	return gain > 0 && until.Sub(next).Hours()*every.Hours() <= maxScanCatchUp.Hours()*gain.Hours()
}

func filterGroups(groups []datadog.ErrorGroup, pattern string) ([]datadog.ErrorGroup, error) {
	if pattern == "" {
		return groups, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(slices.Clone(groups), func(g datadog.ErrorGroup) bool {
		if re.MatchString(g.Kind) || re.MatchString(g.Pattern) {
			return false
		}
		return !slices.ContainsFunc(g.Samples, func(s datadog.ErrorSample) bool { return re.MatchString(s.Message) })
	}), nil
}

func groupFromDatadog(g datadog.ErrorGroup) Group {
	out := Group{
		Fingerprint: g.Fingerprint,
		Service:     oneLine(g.Service, 200),
		Kind:        oneLine(redact.Text(g.Kind), 200),
		Pattern:     oneLine(redact.Text(g.Pattern), 400),
		Frame:       oneLine(redact.Text(g.Frame), 300),
		Count:       g.Count,
		FirstSeen:   stamp(g.FirstSeen),
		LastSeen:    stamp(g.LastSeen),
	}
	for _, s := range g.Samples {
		out.Samples = append(out.Samples, Sample{
			At:      stamp(s.Timestamp),
			Message: text.Truncate(redact.Text(s.Message), sampleMessageChars),
			Stack:   text.TruncateStart(redact.Text(s.Stack), sampleStackChars),
		})
	}
	return out
}
