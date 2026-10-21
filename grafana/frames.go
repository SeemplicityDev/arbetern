package grafana

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/justmike1/arbetern/internal/text"
)

// Frame is one decoded data frame.
type Frame struct {
	Name     string
	Fields   []Field
	Rows     int
	Executed string
	Notices  []string
}

// Field is one column of a frame. Values hold time.Time, json.Number, string,
// bool, nil, or compact JSON text for nested values.
type Field struct {
	Name   string
	Type   string
	Labels map[string]string
	Values []any
}

func decodeFrame(f dsFrame) Frame {
	fr := Frame{Name: f.Schema.Name, Executed: f.Schema.Meta.ExecutedQueryString}
	for _, n := range f.Schema.Meta.Notices {
		if n.Text != "" {
			fr.Notices = append(fr.Notices, n.Text)
		}
	}
	for i, sf := range f.Schema.Fields {
		fld := Field{Name: firstNonEmpty(sf.Config.DisplayNameFromDS, sf.Name), Type: sf.Type, Labels: sf.Labels}
		if i < len(f.Data.Values) {
			fld.Values = make([]any, len(f.Data.Values[i]))
			for j, raw := range f.Data.Values[i] {
				fld.Values[j] = decodeValue(raw, sf.Type)
			}
		}
		if len(fld.Values) > fr.Rows {
			fr.Rows = len(fld.Values)
		}
		fr.Fields = append(fr.Fields, fld)
	}
	return fr
}

func decodeValue(raw json.RawMessage, typ string) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	switch typ {
	case "time":
		var ms float64
		if json.Unmarshal(raw, &ms) == nil {
			return time.UnixMilli(int64(ms)).UTC()
		}
	case "number":
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var n json.Number
		if dec.Decode(&n) == nil {
			return n
		}
	case "string":
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	case "boolean":
		var b bool
		if json.Unmarshal(raw, &b) == nil {
			return b
		}
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) == nil {
		return buf.String()
	}
	return string(raw)
}

// renderFrame writes a frame as a capped table, or, when a long frame is a
// time series, as one summary line per series.
func renderFrame(sb *strings.Builder, f Frame, maxRows int) {
	if f.Rows == 0 {
		sb.WriteString("_no rows_\n")
		return
	}
	timeIdx := -1
	var nums, labels []int
	for i, fld := range f.Fields {
		switch fld.Type {
		case "time":
			if timeIdx < 0 {
				timeIdx = i
				continue
			}
			labels = append(labels, i)
		case "number":
			nums = append(nums, i)
		default:
			labels = append(labels, i)
		}
	}
	if f.Rows > maxRows && timeIdx >= 0 && len(nums) > 0 {
		renderSeries(sb, f, timeIdx, nums, labels)
		return
	}
	renderTable(sb, f, maxRows)
}

func renderTable(sb *strings.Builder, f Frame, maxRows int) {
	cols := make([]string, len(f.Fields))
	for i, fld := range f.Fields {
		cols[i] = fld.Name + seriesLabels(fld.Labels)
	}
	fmt.Fprintf(sb, "%s\n", strings.Join(cols, " | "))
	n := f.Rows
	if n > maxRows {
		n = maxRows
	}
	for r := 0; r < n; r++ {
		cells := make([]string, len(f.Fields))
		for i, fld := range f.Fields {
			if r < len(fld.Values) {
				cells[i] = formatCell(fld.Values[r])
			}
		}
		sb.WriteString(strings.Join(cells, " | "))
		sb.WriteString("\n")
	}
	if f.Rows > n {
		fmt.Fprintf(sb, "_… %d more rows not shown (first %d of %d)._\n", f.Rows-n, n, f.Rows)
	}
}

type seriesStats struct {
	name          string
	n             int
	min, max, sum float64
	last          float64
	first, lastT  time.Time
}

const maxSeriesLines = 25

// renderSeries groups a long time-series frame by its label columns and
// summarises each numeric column per group.
func renderSeries(sb *strings.Builder, f Frame, timeIdx int, nums, labels []int) {
	ts := f.Fields[timeIdx].Values
	stats := map[string]*seriesStats{}
	var order []string
	for _, ni := range nums {
		fld := f.Fields[ni]
		for r, raw := range fld.Values {
			v, ok := toFloat(raw)
			if !ok {
				continue
			}
			parts := make([]string, 0, len(labels)+1)
			for _, li := range labels {
				if r < len(f.Fields[li].Values) {
					parts = append(parts, f.Fields[li].Name+"="+text.Truncate(formatCell(f.Fields[li].Values[r]), 60))
				}
			}
			key := strings.Join(parts, ", ")
			name := fld.Name + seriesLabels(fld.Labels)
			if key != "" {
				name += " {" + key + "}"
			}
			st := stats[name]
			if st == nil {
				st = &seriesStats{name: name, min: math.Inf(1), max: math.Inf(-1)}
				stats[name] = st
				order = append(order, name)
			}
			var t time.Time
			if r < len(ts) {
				t, _ = ts[r].(time.Time)
			}
			st.n++
			st.sum += v
			st.min = math.Min(st.min, v)
			st.max = math.Max(st.max, v)
			if st.first.IsZero() || t.Before(st.first) {
				st.first = t
			}
			if !t.Before(st.lastT) {
				st.lastT, st.last = t, v
			}
		}
	}
	list := make([]*seriesStats, 0, len(order))
	for _, k := range order {
		list = append(list, stats[k])
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].sum/float64(list[i].n) > list[j].sum/float64(list[j].n) })
	fmt.Fprintf(sb, "_%d rows summarised as %d series (time column %q), highest average first:_\n", f.Rows, len(list), f.Fields[timeIdx].Name)
	for i, st := range list {
		if i == maxSeriesLines {
			fmt.Fprintf(sb, "_… %d more series not shown._\n", len(list)-i)
			break
		}
		fmt.Fprintf(sb, "• %s — last %s · avg %s · min %s · max %s · sum %s (%d points, %s → %s)\n",
			st.name, formatFloat(st.last), formatFloat(st.sum/float64(st.n)), formatFloat(st.min), formatFloat(st.max),
			formatFloat(st.sum), st.n, formatTime(st.first), formatTime(st.lastT))
	}
}

func seriesLabels(l map[string]string) string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + l[k]
	}
	return " {" + strings.Join(parts, ", ") + "}"
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	case float64:
		return n, true
	}
	return 0, false
}

func formatCell(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case time.Time:
		return formatTime(x)
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return formatFloat(f)
		}
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	case string:
		return text.Truncate(strings.Join(strings.Fields(x), " "), 200)
	}
	return text.Truncate(fmt.Sprint(v), 200)
}

func formatFloat(f float64) string {
	switch {
	case math.IsNaN(f) || math.IsInf(f, 0):
		return strconv.FormatFloat(f, 'f', -1, 64)
	case f == math.Trunc(f) && math.Abs(f) < 1e15:
		return strconv.FormatInt(int64(f), 10)
	case math.Abs(f) >= 1:
		return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(f, 'f', 2, 64), "0"), ".")
	}
	return strconv.FormatFloat(f, 'g', 4, 64)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	if t.Nanosecond() != 0 {
		return t.UTC().Format("2006-01-02 15:04:05.000")
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}
