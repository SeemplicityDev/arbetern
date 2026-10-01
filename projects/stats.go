package projects

import (
	"slices"
	"time"
)

func computeStats(l *Ledger, p *Project, now time.Time) Stats {
	t := l.Totals
	s := Stats{
		Sessions:             t.Sessions,
		FailedSessions:       t.FailedSessions,
		NoFix:                t.NoFix,
		PRsOpened:            t.PRsOpened,
		PRsMerged:            t.PRsMerged,
		PRsClosed:            t.PRsClosed,
		MergeRate:            ratio(t.PRsMerged, t.PRsMerged+t.PRsClosed),
		MedianMergeHours:     median(t.MergeHours),
		MedianSessionMinutes: median(t.SessionMinutes),
		LinesChanged:         t.LinesChanged,
		ErrorGroups:          len(l.Tracks),
		Resolved:             t.Resolved,
		Recurred:             t.Recurred,
		ResolutionRate:       ratio(t.Resolved, t.Resolved+t.Recurred),
		Backlog:              len(l.backlog(p.Signal.key(), p.Limits.effective(), now, 0)),
		UpdatedAt:            stamp(now),
	}
	s.PRsOpen, s.ActiveSessions = l.counts()
	s.Daily = make([]Day, 0, statsDays)
	today := now.UTC()
	for i := statsDays - 1; i >= 0; i-- {
		date := today.AddDate(0, 0, -i).Format(time.DateOnly)
		d := Day{Date: date}
		if stored := l.Daily[date]; stored != nil {
			d = *stored
			d.Date = date
		}
		s.Daily = append(s.Daily, d)
	}
	return s
}

func ratio(n, of int) float64 {
	if of <= 0 {
		return 0
	}
	return round(float64(n)/float64(of), 4)
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return round(s[mid], 2)
	}
	return round((s[mid-1]+s[mid])/2, 2)
}
