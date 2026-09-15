package metrics

import (
	"sort"
	"time"
)

// TurnRow is one dimension's turn rollup, reduced for display.
type TurnRow struct {
	Key           string     `json:"key"`
	Latency       StatView   `json:"latency"`
	FirstResponse StatView   `json:"first_response"`
	AvgRounds     float64    `json:"avg_rounds"`
	AvgToolCalls  float64    `json:"avg_tool_calls"`
	ModelPct      float64    `json:"model_pct"`
	ToolPct       float64    `json:"tool_pct"`
	TokensPerSec  float64    `json:"tokens_per_sec"`
	Outcomes      []CountRow `json:"outcomes,omitempty"`
}

// CallRow is one model's provider round-trip rollup.
type CallRow struct {
	Key         string   `json:"key"`
	Latency     StatView `json:"latency"`
	Attempts    int64    `json:"attempts"`
	Retried     int64    `json:"retried"`
	RetryPct    float64  `json:"retry_pct"`
	RateLimited int64    `json:"rate_limited"`
}

// ToolRow is one tool's latency rollup.
type ToolRow struct {
	Key     string   `json:"key"`
	Latency StatView `json:"latency"`
}

// CountRow is a plain labelled count.
type CountRow struct {
	Key   string  `json:"key"`
	Count int64   `json:"count"`
	Pct   float64 `json:"pct"`
}

// DailyRow is one UTC day of turn latency.
type DailyRow struct {
	Key     string   `json:"key"`
	Latency StatView `json:"latency"`
}

// Summary is the aggregated performance view served to the UI for a trailing
// window.
type Summary struct {
	Days  int    `json:"days"`
	Since string `json:"since"`
	// Turns is the window total; the per-dimension rows below span every
	// retained month with activity in the window, which is exact for the
	// common full-window cases and generous for a sub-month one.
	Turns     TurnRow    `json:"turns"`
	ByAgent   []TurnRow  `json:"by_agent"`
	BySource  []TurnRow  `json:"by_source"`
	ByModel   []TurnRow  `json:"by_model"`
	ByOutcome []CountRow `json:"by_outcome"`
	Calls     CallRow    `json:"calls"`
	ByCall    []CallRow  `json:"by_call"`
	ByTool    []ToolRow  `json:"by_tool"`
	Daily     []DailyRow `json:"daily"`
	Slowest   []Turn     `json:"slowest"`
	Recent    []Turn     `json:"recent"`
	Months    []string   `json:"months"`
	Runtime   Runtime    `json:"runtime"`
	Queue     any        `json:"queue,omitempty"`
	Deps      any        `json:"deps,omitempty"`
}

// Summarize aggregates the trailing `days` (UTC) into a single view. days<=0
// means the full retained history.
func (s *Store) Summarize(days int) Summary {
	// Taken before the lock: reading the memory stats stops the world briefly.
	rt := snapshotRuntime(s.startAt)
	// The two hooks reach into the queue and the LLM client, which hold locks
	// of their own. Calling them while this store is locked would tie three
	// lock orders together for no reason, so they are resolved here and
	// invoked once the aggregation has let go.
	s.mu.RLock()
	queueFn, depsFn := s.queueStats, s.dependencies
	s.mu.RUnlock()

	out := s.aggregate(days, rt)
	if queueFn != nil {
		out.Queue = queueFn()
	}
	if depsFn != nil {
		out.Deps = depsFn()
	}
	return out
}

func (s *Store) aggregate(days int, rt Runtime) Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var since time.Time
	if days > 0 {
		since = time.Now().UTC().AddDate(0, 0, -days+1).Truncate(24 * time.Hour)
	}
	out := Summary{Days: days, Runtime: rt}
	if !since.IsZero() {
		out.Since = since.Format("2006-01-02")
	}

	total := &TurnStat{}
	agents := map[string]*TurnStat{}
	sources := map[string]*TurnStat{}
	models := map[string]*TurnStat{}
	calls := map[string]*CallStat{}
	tools := map[string]*Stat{}
	daily := map[string]*TurnStat{}

	for mk, m := range s.months {
		out.Months = append(out.Months, mk)
		for d, t := range m.Days {
			if !since.IsZero() && d < out.Since {
				continue
			}
			total.merge(t)
			turnBucket(daily, d).merge(t)
		}
		mergeTurns(agents, m.Agents)
		mergeTurns(sources, m.Sources)
		mergeTurns(models, m.Models)
		for k, c := range m.Calls {
			dst := calls[k]
			if dst == nil {
				dst = &CallStat{}
				calls[k] = dst
			}
			dst.merge(c)
		}
		for k, st := range m.Tools {
			dst := tools[k]
			if dst == nil {
				dst = &Stat{}
				tools[k] = dst
			}
			dst.merge(st)
		}
	}
	sort.Strings(out.Months)

	out.Turns = turnRow("all", total, true)
	out.ByAgent = turnRows(agents)
	out.BySource = turnRows(sources)
	out.ByModel = turnRows(models)
	out.ByOutcome = outcomeRows(total)

	allCalls := &CallStat{}
	for _, c := range calls {
		allCalls.merge(c)
	}
	out.Calls = callRow("all", allCalls)
	out.ByCall = callRows(calls)
	out.ByTool = toolRows(tools)
	out.Daily = dailyRows(daily)
	windowed := recentSince(s.recent, since)
	out.Slowest = slowest(windowed)
	if len(windowed) > maxSummaryRecent {
		windowed = windowed[:maxSummaryRecent]
	}
	out.Recent = windowed
	return out
}

func mergeTurns(dst, src map[string]*TurnStat) {
	for k, t := range src {
		turnBucket(dst, k).merge(t)
	}
}

func turnRow(key string, t *TurnStat, histogram bool) TurnRow {
	r := TurnRow{Key: key, Latency: t.Latency.view(histogram), FirstResponse: t.FirstResponse.view(false)}
	if n := t.Latency.Count; n > 0 {
		r.AvgRounds = round2(float64(t.Rounds) / float64(n))
		r.AvgToolCalls = round2(float64(t.ToolCalls) / float64(n))
	}
	if t.Latency.SumMS > 0 {
		r.ModelPct = round2(float64(t.ModelMS) / float64(t.Latency.SumMS) * 100)
		r.ToolPct = round2(float64(t.ToolMS) / float64(t.Latency.SumMS) * 100)
	}
	if t.ModelMS > 0 {
		r.TokensPerSec = round2(float64(t.OutputTokens) / (float64(t.ModelMS) / 1000))
	}
	if histogram {
		r.Outcomes = outcomeRows(t)
	}
	return r
}

func turnRows(m map[string]*TurnStat) []TurnRow {
	out := make([]TurnRow, 0, len(m))
	for k, t := range m {
		if t.Latency.Count == 0 {
			continue
		}
		out = append(out, turnRow(k, t, false))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Latency.Count != out[j].Latency.Count {
			return out[i].Latency.Count > out[j].Latency.Count
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func outcomeRows(t *TurnStat) []CountRow {
	if len(t.Outcomes) == 0 {
		return nil
	}
	var total int64
	for _, n := range t.Outcomes {
		total += n
	}
	out := make([]CountRow, 0, len(t.Outcomes))
	for k, n := range t.Outcomes {
		row := CountRow{Key: k, Count: n}
		if total > 0 {
			row.Pct = round2(float64(n) / float64(total) * 100)
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

func callRow(key string, c *CallStat) CallRow {
	r := CallRow{Key: key, Latency: c.Latency.view(false), Attempts: c.Attempts, Retried: c.Retried, RateLimited: c.RateLimited}
	if c.Latency.Count > 0 {
		r.RetryPct = round2(float64(c.Retried) / float64(c.Latency.Count) * 100)
	}
	return r
}

func callRows(m map[string]*CallStat) []CallRow {
	out := make([]CallRow, 0, len(m))
	for k, c := range m {
		if c.Latency.Count == 0 {
			continue
		}
		out = append(out, callRow(k, c))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Latency.Count > out[j].Latency.Count })
	return out
}

func toolRows(m map[string]*Stat) []ToolRow {
	out := make([]ToolRow, 0, len(m))
	for k, st := range m {
		if st.Count == 0 {
			continue
		}
		out = append(out, ToolRow{Key: k, Latency: st.view(false)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Latency.Count != out[j].Latency.Count {
			return out[i].Latency.Count > out[j].Latency.Count
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func dailyRows(m map[string]*TurnStat) []DailyRow {
	out := make([]DailyRow, 0, len(m))
	for k, t := range m {
		out = append(out, DailyRow{Key: k, Latency: t.Latency.view(false)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func recentSince(all []Turn, since time.Time) []Turn {
	out := make([]Turn, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		if !since.IsZero() && all[i].At.Before(since) {
			continue
		}
		out = append(out, all[i])
	}
	return out
}

func slowest(recent []Turn) []Turn {
	out := append([]Turn(nil), recent...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].DurationMS > out[j].DurationMS })
	if len(out) > maxSlowestTurns {
		out = out[:maxSlowestTurns]
	}
	return out
}

func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }
