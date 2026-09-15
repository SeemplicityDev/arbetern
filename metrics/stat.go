package metrics

import "sort"

// LatencyBuckets are the inclusive upper bounds, in milliseconds, of the
// histogram every latency rollup carries. Keeping the distribution instead of
// raw samples is what lets percentiles survive both aggregation across
// replicas and the month rollup: two histograms merge by adding their counts.
var LatencyBuckets = []int64{50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 120000, 300000, 600000}

// Stat is a latency distribution plus its failure count.
type Stat struct {
	Count   int64   `json:"count"`
	Failed  int64   `json:"failed"`
	SumMS   int64   `json:"sum_ms"`
	MinMS   int64   `json:"min_ms"`
	MaxMS   int64   `json:"max_ms"`
	Buckets []int64 `json:"buckets"`
}

func bucketOf(ms int64) int {
	i := sort.Search(len(LatencyBuckets), func(i int) bool { return ms <= LatencyBuckets[i] })
	return i
}

func (s *Stat) observe(ms int64, failed bool) {
	if ms < 0 {
		ms = 0
	}
	if len(s.Buckets) != len(LatencyBuckets)+1 {
		s.Buckets = make([]int64, len(LatencyBuckets)+1)
	}
	if s.Count == 0 || ms < s.MinMS {
		s.MinMS = ms
	}
	if ms > s.MaxMS {
		s.MaxMS = ms
	}
	s.Count++
	s.SumMS += ms
	s.Buckets[bucketOf(ms)]++
	if failed {
		s.Failed++
	}
}

func (s *Stat) merge(o *Stat) {
	if o == nil || o.Count == 0 {
		return
	}
	if len(s.Buckets) != len(LatencyBuckets)+1 {
		s.Buckets = make([]int64, len(LatencyBuckets)+1)
	}
	if s.Count == 0 || o.MinMS < s.MinMS {
		s.MinMS = o.MinMS
	}
	if o.MaxMS > s.MaxMS {
		s.MaxMS = o.MaxMS
	}
	s.Count += o.Count
	s.Failed += o.Failed
	s.SumMS += o.SumMS
	for i := range o.Buckets {
		if i < len(s.Buckets) {
			s.Buckets[i] += o.Buckets[i]
		}
	}
}

// percentile interpolates within the bucket the rank falls into, so a
// distribution kept as counts still yields a usable p50/p95.
func (s *Stat) percentile(p float64) int64 {
	if s.Count == 0 || len(s.Buckets) == 0 {
		return 0
	}
	target := p * float64(s.Count)
	var cum float64
	for i, n := range s.Buckets {
		if n == 0 {
			continue
		}
		if cum+float64(n) < target {
			cum += float64(n)
			continue
		}
		if i >= len(LatencyBuckets) {
			return s.MaxMS
		}
		var lo int64
		if i > 0 {
			lo = LatencyBuckets[i-1]
		}
		hi := LatencyBuckets[i]
		if lo < s.MinMS {
			lo = s.MinMS
		}
		if hi > s.MaxMS {
			hi = s.MaxMS
		}
		if hi <= lo {
			return hi
		}
		frac := (target - cum) / float64(n)
		return lo + int64(frac*float64(hi-lo))
	}
	return s.MaxMS
}

// StatView is the shape served to the UI: the distribution reduced to the few
// numbers a dashboard actually plots.
type StatView struct {
	Count   int64   `json:"count"`
	Failed  int64   `json:"failed"`
	FailPct float64 `json:"fail_pct"`
	AvgMS   int64   `json:"avg_ms"`
	P50MS   int64   `json:"p50_ms"`
	P90MS   int64   `json:"p90_ms"`
	P95MS   int64   `json:"p95_ms"`
	P99MS   int64   `json:"p99_ms"`
	MinMS   int64   `json:"min_ms"`
	MaxMS   int64   `json:"max_ms"`
	Buckets []int64 `json:"buckets,omitempty"`
	Bounds  []int64 `json:"bounds,omitempty"`
}

func (s *Stat) view(withHistogram bool) StatView {
	v := StatView{Count: s.Count, Failed: s.Failed, MinMS: s.MinMS, MaxMS: s.MaxMS}
	if s.Count == 0 {
		return v
	}
	v.FailPct = float64(int64(float64(s.Failed)/float64(s.Count)*10000+0.5)) / 100
	v.AvgMS = s.SumMS / s.Count
	v.P50MS = s.percentile(0.50)
	v.P90MS = s.percentile(0.90)
	v.P95MS = s.percentile(0.95)
	v.P99MS = s.percentile(0.99)
	if withHistogram {
		v.Buckets = append([]int64(nil), s.Buckets...)
		v.Bounds = LatencyBuckets
	}
	return v
}
