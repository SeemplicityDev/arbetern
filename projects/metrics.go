package projects

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/justmike1/arbetern/datadog"
	"github.com/justmike1/arbetern/internal/redact"
	"github.com/justmike1/arbetern/internal/text"
)

const logSearchSpecials = `+-=&|><!(){}[]^"~*?:\/ `

type seriesSum struct {
	site  string
	tags  []string
	value float64
	kept  float64
	share float64
}

func (m seriesSum) key() string { return m.site + "\x00" + strings.Join(m.tags, ",") }

func seriesFingerprint(site string, tags []string) string {
	sum := sha256.Sum256([]byte("metric\x00" + site + "\x00" + strings.Join(tags, ",")))
	return hex.EncodeToString(sum[:8])
}

func seriesTags(s datadog.MetricSeries) []string {
	tags := slices.Clone(s.TagSet)
	if len(tags) == 0 && s.Scope != "" && s.Scope != "*" {
		tags = strings.Split(s.Scope, ",")
	}
	for i := range tags {
		tags[i] = strings.TrimSpace(tags[i])
	}
	tags = slices.DeleteFunc(tags, func(t string) bool { return t == "" })
	slices.Sort(tags)
	return slices.Compact(tags)
}

func metricSums(ctx context.Context, client *datadog.Client, query string, from, to time.Time) (map[string][]string, map[string]float64, error) {
	resp, err := client.QueryMetrics(ctx, query, strconv.FormatInt(from.Unix(), 10), strconv.FormatInt(to.Unix(), 10))
	if err != nil {
		return nil, nil, err
	}
	if len(resp.Series) > maxSeriesPerSite {
		return nil, nil, fmt.Errorf("the query returned %d series, more than %d", len(resp.Series), maxSeriesPerSite)
	}
	tags, sums := map[string][]string{}, map[string]float64{}
	for _, s := range resp.Series {
		t := seriesTags(s)
		k := strings.Join(t, ",")
		tags[k] = t
		// Every point of a series adds up to its total over the window, whatever rollup the query asks for.
		for _, pt := range s.PointList {
			if len(pt) >= 2 && pt[1] != nil && !math.IsNaN(*pt[1]) && !math.IsInf(*pt[1], 0) {
				sums[k] += *pt[1]
			}
		}
	}
	return tags, sums, nil
}

func (r *Registry) scanMetrics(ctx context.Context, p *Project, led *Ledger, limits Limits, out *tickOutcome, force bool) (*Ledger, error) {
	now := time.Now().UTC()
	signal := p.Signal.key()
	if !force && led.Signal == signal && now.Sub(parseTime(led.Cursor)) < metricRescan {
		return led, nil
	}
	window := p.Signal.window()
	from := now.Add(-window)
	var all []seriesSum
	for _, site := range p.Signal.Sites {
		client := r.datadogFor(site)
		if client == nil {
			return led, fmt.Errorf("this deployment has no Datadog credentials for site %s", site)
		}
		tags, values, err := metricSums(ctx, client, p.Signal.Query, from, now)
		if err != nil {
			return led, fmt.Errorf("querying Datadog %s metrics: %w", strings.ToUpper(site), err)
		}
		kept := map[string]float64{}
		if p.Signal.KeptQuery != "" {
			if _, kept, err = metricSums(ctx, client, p.Signal.KeptQuery, from, now); err != nil {
				return led, fmt.Errorf("querying Datadog %s metrics (kept_query): %w", strings.ToUpper(site), err)
			}
		}
		for k, v := range values {
			m := seriesSum{site: site, tags: tags[k], value: v, kept: kept[k]}
			if total := m.value + m.kept; p.Signal.KeptQuery != "" && total > 0 {
				m.share = m.value / total
			}
			all = append(all, m)
		}
	}
	groups, err := rankSeries(all, p.Signal, now)
	if err != nil {
		return led, err
	}
	out.scanned, out.groups = len(all), len(groups)
	updated, _, err := r.mergeLedger(ctx, p.ID, led, func(l *Ledger) bool { return l.applyMetricScan(signal, groups, window, limits, now) })
	if err != nil {
		return led, fmt.Errorf("recording the scan: %w", err)
	}
	return updated, nil
}

func rankSeries(all []seriesSum, s Signal, now time.Time) ([]Group, error) {
	var re *regexp.Regexp
	if s.Pattern != "" {
		var err error
		if re, err = regexp.Compile(s.Pattern); err != nil {
			return nil, fmt.Errorf("signal.pattern: %w", err)
		}
	}
	var byShare, byValue, fill []seriesSum
	for _, m := range all {
		if re != nil && !re.MatchString(strings.Join(m.tags, ",")) {
			continue
		}
		switch {
		case s.ShareThreshold > 0 && m.share > s.ShareThreshold:
			byShare = append(byShare, m)
		case s.Threshold > 0 && m.value > s.Threshold:
			byValue = append(byValue, m)
		case s.Fill && m.value > 0:
			fill = append(fill, m)
		}
	}
	bigger := func(a, b seriesSum) int {
		return cmp.Or(cmp.Compare(b.value, a.value), strings.Compare(a.key(), b.key()))
	}
	slices.SortFunc(byShare, func(a, b seriesSum) int { return cmp.Or(cmp.Compare(b.share, a.share), bigger(a, b)) })
	slices.SortFunc(byValue, bigger)
	slices.SortFunc(fill, bigger)
	at := stamp(now)
	groups := make([]Group, 0, min(len(byShare)+len(byValue)+len(fill), maxCandidates))
	add := func(m seriesSum, reason string) {
		if len(groups) < maxCandidates {
			groups = append(groups, Group{
				Fingerprint: seriesFingerprint(m.site, m.tags),
				Site:        m.site,
				Tags:        m.tags,
				Value:       round(m.value, 2),
				Kept:        round(m.kept, 2),
				Share:       round(m.share, 6),
				Reason:      reason,
				Count:       int(math.Round(m.value)),
				FirstSeen:   at,
				LastSeen:    at,
			})
		}
	}
	// Alternating keeps a large loss under the share threshold from waiting behind every share breach, and the reverse.
	for i := 0; i < max(len(byShare), len(byValue)); i++ {
		if i < len(byShare) {
			add(byShare[i], reasonShare)
		}
		if i < len(byValue) {
			add(byValue[i], reasonThreshold)
		}
	}
	for _, m := range fill {
		add(m, reasonFill)
	}
	return groups, nil
}

func tagValues(tags []string) map[string]string {
	out := make(map[string]string, len(tags))
	for _, t := range tags {
		if k, v, ok := strings.Cut(t, ":"); ok {
			out[k] = v
		}
	}
	return out
}

func logWildcard(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch {
		// Tags store spaces and other characters as underscores, so an underscore matches any one character.
		case r == '_':
			b.WriteByte('?')
		case strings.ContainsRune(logSearchSpecials, r):
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func evidenceQuery(ev *Evidence, tags []string) string {
	values := tagValues(tags)
	parts := []string{ev.Query}
	keys := make([]string, 0, len(ev.Attributes))
	for k := range ev.Attributes {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if v := values[k]; v != "" && !strings.EqualFold(v, "n/a") {
			parts = append(parts, ev.Attributes[k]+":"+logWildcard(v))
		}
	}
	return strings.Join(parts, " ")
}

func (r *Registry) evidence(ctx context.Context, p *Project, g Group, now time.Time) (Group, error) {
	ev := p.Signal.Evidence
	if ev == nil || !p.Signal.metric() {
		return g, nil
	}
	client := r.datadogFor(g.Site)
	if client == nil {
		return g, fmt.Errorf("this deployment has no Datadog credentials for site %s", g.Site)
	}
	query := evidenceQuery(ev, g.Tags)
	from, to := now.Add(-p.Signal.window()).Format(time.RFC3339), now.Format(time.RFC3339)
	g.EvidenceQuery, g.Cause, g.Causes = query, "", nil
	top := ""
	if ev.GroupBy != "" {
		agg, err := client.LogsAggregate(ctx, query, from, to, []datadog.AggregateCompute{{Aggregation: "count"}},
			[]datadog.AggregateGroupBy{{Facet: ev.GroupBy, Limit: maxEvidenceCauses, Sort: &datadog.AggregateSort{Aggregation: "count", Order: "desc", Type: "measure"}}})
		if err != nil {
			return g, fmt.Errorf("counting the evidence logs: %w", err)
		}
		buckets := slices.Clone(agg.Data.Buckets)
		slices.SortStableFunc(buckets, func(a, b datadog.AggregateBucket) int { return cmp.Compare(b.Computes["c0"], a.Computes["c0"]) })
		for _, b := range buckets {
			raw, ok := b.By[ev.GroupBy]
			if !ok || raw == nil {
				continue
			}
			v := strings.TrimSpace(fmt.Sprint(raw))
			if v == "" {
				continue
			}
			if top == "" {
				top = v
			}
			g.Causes = append(g.Causes, Cause{Value: oneLine(redact.Text(v), 400), Count: int(b.Computes["c0"])})
		}
		if len(g.Causes) > 0 {
			g.Cause = g.Causes[0].Value
		}
	}
	res, err := client.SearchLogs(ctx, query, from, to, evidenceSearchLimit)
	if err != nil {
		return g, fmt.Errorf("searching the evidence logs: %w", err)
	}
	g.Samples = evidenceSamplesFrom(res.Data, ev, top)
	return g, nil
}

func evidenceSamplesFrom(entries []datadog.LogEntry, ev *Evidence, cause string) []Sample {
	var picked []datadog.LogEntry
	if cause != "" {
		path := strings.TrimPrefix(ev.GroupBy, "@")
		for _, e := range entries {
			if v, ok := lookupAttr(e.Attributes.Attributes, path); ok && strings.TrimSpace(fmt.Sprint(v)) == cause {
				picked = append(picked, e)
			}
			if len(picked) == evidenceSamples {
				break
			}
		}
	}
	if len(picked) == 0 {
		picked = entries[:min(len(entries), evidenceSamples)]
	}
	samples := make([]Sample, 0, len(picked))
	for _, e := range picked {
		at := e.Attributes.Timestamp
		if t := parseTime(at); !t.IsZero() {
			at = stamp(t)
		}
		samples = append(samples, Sample{
			At:      at,
			Message: text.Truncate(redact.Text(e.Attributes.Message), sampleMessageChars),
			Fields:  text.Truncate(redact.Text(renderFields(e.Attributes.Attributes, ev.Fields)), evidenceSampleChars),
		})
	}
	return samples
}

func lookupAttr(attrs map[string]any, path string) (any, bool) {
	if v, ok := attrs[path]; ok {
		return v, true
	}
	head, rest, found := strings.Cut(path, ".")
	if !found {
		return nil, false
	}
	next, ok := attrs[head].(map[string]any)
	if !ok {
		return nil, false
	}
	return lookupAttr(next, rest)
}

func indentJSON(v any, prefix string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent(prefix, "  ")
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderFields(attrs map[string]any, fields []string) string {
	if len(fields) == 0 {
		return indentJSON(attrs, "")
	}
	var b strings.Builder
	b.WriteString("{")
	n := 0
	for _, f := range fields {
		path := strings.TrimPrefix(f, "@")
		v, ok := lookupAttr(attrs, path)
		if !ok {
			continue
		}
		if n > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "\n  %s: %s", indentJSON(path, ""), indentJSON(v, "  "))
		n++
	}
	b.WriteString("\n}")
	return b.String()
}
