// Package series describes how a time series moved over its window.
package series

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Point is one timestamped value.
type Point struct {
	At    time.Time
	Value float64
}

// MaxShapes caps how many series of one tool result carry a Shape line.
const MaxShapes = 10

const (
	maxSlots  = 16
	minEffect = 1.0
	minChange = 0.15
)

var slotWidths = []time.Duration{
	time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 30 * time.Minute,
	time.Hour, 2 * time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour, 48 * time.Hour, 7 * 24 * time.Hour,
}

type shift struct {
	at            time.Time
	before, after float64
}

// Shape reports level shifts and per-slot means, e.g. "level 48 → 99 at 14:41 · 30m means from 12:00 UTC: 47.9 48.1 …".
func Shape(points []Point) string {
	pts := make([]Point, 0, len(points))
	for _, p := range points {
		if !p.At.IsZero() && !math.IsNaN(p.Value) && !math.IsInf(p.Value, 0) {
			pts = append(pts, p)
		}
	}
	if len(pts) < 2 {
		return ""
	}
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].At.Before(pts[j].At) })
	layout := "15:04"
	if !sameDay(pts[0].At, pts[len(pts)-1].At) {
		layout = "01-02 15:04"
	}
	var sb strings.Builder
	if found := shifts(pts); len(found) == 0 {
		sb.WriteString("no level shift")
	} else {
		sb.WriteString("level ")
		for i, s := range found {
			if i > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "%s → %s at %s", compact(s.before), compact(s.after), s.at.UTC().Format(layout))
		}
	}
	width, start, means := slotMeans(pts)
	fmt.Fprintf(&sb, " · %s means from %s UTC:", Duration(width), start.UTC().Format(layout))
	for _, m := range means {
		sb.WriteByte(' ')
		if math.IsNaN(m) {
			sb.WriteString("·")
		} else {
			sb.WriteString(compact(m))
		}
	}
	return sb.String()
}

func shifts(pts []Point) []shift {
	vals := make([]float64, len(pts))
	for i, p := range pts {
		vals[i] = p.Value
	}
	minSeg := max(3, len(vals)/20)
	var cuts []int
	var walk func(lo, hi, depth int)
	walk = func(lo, hi, depth int) {
		k, ok := bestSplit(vals[lo:hi], minSeg)
		if !ok {
			return
		}
		cuts = append(cuts, lo+k)
		if depth > 0 {
			walk(lo, lo+k, depth-1)
			walk(lo+k, hi, depth-1)
		}
	}
	walk(0, len(vals), 1)
	sort.Ints(cuts)
	bounds := append(append([]int{0}, cuts...), len(vals))
	found := make([]shift, len(cuts))
	for i, k := range cuts {
		found[i] = shift{at: pts[k].At, before: mean(vals[bounds[i]:k]), after: mean(vals[k:bounds[i+2]])}
	}
	return found
}

func bestSplit(v []float64, minSeg int) (int, bool) {
	n := len(v)
	if n < 2*minSeg {
		return 0, false
	}
	sum, sq := make([]float64, n+1), make([]float64, n+1)
	for i, x := range v {
		sum[i+1] = sum[i] + x
		sq[i+1] = sq[i] + x*x
	}
	best, k := -1.0, 0
	for i := minSeg; i <= n-minSeg; i++ {
		d := sum[i]/float64(i) - (sum[n]-sum[i])/float64(n-i)
		if between := float64(i) * float64(n-i) / float64(n) * d * d; between > best {
			best, k = between, i
		}
	}
	before, after := sum[k]/float64(k), (sum[n]-sum[k])/float64(n-k)
	delta := math.Abs(after - before)
	if delta == 0 || delta < minChange*math.Max(math.Abs(before), math.Abs(after)) {
		return 0, false
	}
	if within := sq[n] - sum[n]*sum[n]/float64(n) - best; n > 2 && within > 0 && delta < minEffect*math.Sqrt(within/float64(n-2)) {
		return 0, false
	}
	return k, true
}

func slotMeans(pts []Point) (time.Duration, time.Time, []float64) {
	first, last := pts[0].At, pts[len(pts)-1].At
	step := medianStep(pts)
	width := slotWidths[len(slotWidths)-1]
	for _, w := range slotWidths {
		if w >= step && int(last.Sub(first.Truncate(w))/w) < maxSlots {
			width = w
			break
		}
	}
	start := first.Truncate(width)
	n := int(last.Sub(start)/width) + 1
	sums, counts := make([]float64, n), make([]int, n)
	for _, p := range pts {
		i := int(p.At.Sub(start) / width)
		sums[i] += p.Value
		counts[i]++
	}
	means := make([]float64, n)
	for i := range means {
		means[i] = math.NaN()
		if counts[i] > 0 {
			means[i] = sums[i] / float64(counts[i])
		}
	}
	return width, start, means
}

func medianStep(pts []Point) time.Duration {
	gaps := make([]time.Duration, 0, len(pts)-1)
	for i := 1; i < len(pts); i++ {
		gaps = append(gaps, pts[i].At.Sub(pts[i-1].At))
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	return gaps[len(gaps)/2]
}

func mean(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func sameDay(a, b time.Time) bool {
	y1, m1, d1 := a.UTC().Date()
	y2, m2, d2 := b.UTC().Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}

// Duration renders d compactly, e.g. 30s, 5m, 2h or 1d.
func Duration(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}

func compact(v float64) string {
	units := []string{"", "k", "M", "B", "T"}
	i := 0
	for i < len(units)-1 && math.Abs(v) >= 999.5 {
		v /= 1000
		i++
	}
	return strconv.FormatFloat(v, 'g', 3, 64) + units[i]
}
