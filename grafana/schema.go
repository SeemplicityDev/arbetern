package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// TableInfo is one table of a ClickHouse datasource.
type TableInfo struct {
	Database     string
	Name         string
	Engine       string
	Rows         *float64
	Bytes        *float64
	PartitionKey string
	SortingKey   string
	TTL          string
}

// ColumnInfo is one column of a table.
type ColumnInfo struct {
	Database       string
	Name           string
	Type           string
	Comment        string
	InSortingKey   bool
	InPartitionKey bool
}

// SchemaResult describes the tables of one datasource, or one table.
type SchemaResult struct {
	Datasource  Datasource
	Table       string
	Tables      []TableInfo
	Columns     []ColumnInfo
	Unreachable []string
}

var ttlRe = regexp.MustCompile(`\bTTL\s+(.+?)(?:\s+SETTINGS\s|$)`)

// SQLSchema lists a ClickHouse datasource's tables or describes one, using the first datasource that answers.
func (c *Client) SQLSchema(ctx context.Context, datasource, database, table string) (*SchemaResult, error) {
	var refs []string
	if strings.TrimSpace(datasource) != "" {
		refs = []string{datasource}
	}
	targets, err := c.clickHouseTargets(ctx, refs)
	if err != nil {
		return nil, err
	}
	database, table = strings.TrimSpace(database), strings.TrimSpace(table)
	if database == "" && strings.Count(table, ".") == 1 {
		parts := strings.SplitN(table, ".", 2)
		database, table = parts[0], parts[1]
	}
	conds := []string{"database NOT IN ('system', 'INFORMATION_SCHEMA', 'information_schema')"}
	if database != "" {
		conds = []string{"database = " + chString(database)}
	}
	if table != "" {
		conds = append(conds, "name = "+chString(table))
	}
	queries := []namedSQL{{ref: "tables", sql: "SELECT database, name, engine, total_rows, total_bytes, partition_key, sorting_key, engine_full FROM system.tables WHERE " +
		strings.Join(conds, " AND ") + " ORDER BY database, name LIMIT 1000"}}
	if table != "" {
		colConds := []string{"table = " + chString(table)}
		if database != "" {
			colConds = append(colConds, "database = "+chString(database))
		}
		queries = append(queries, namedSQL{ref: "columns", sql: "SELECT database, name, type, comment, is_in_sorting_key, is_in_partition_key FROM system.columns WHERE " +
			strings.Join(colConds, " AND ") + " ORDER BY database, position LIMIT 2000"})
	}
	now := time.Now()
	res := &SchemaResult{Table: table}
	var lastErr error
	for _, ds := range targets {
		out, err := c.runSQL(ctx, ds, now.Add(-time.Hour), now, time.Minute, queries)
		if err == nil && out["tables"].Err == unreachableNotice {
			res.Unreachable = append(res.Unreachable, ds.Name)
			continue
		}
		if err != nil {
			lastErr = err
			continue
		}
		if e := out["tables"].Err; e != "" {
			return nil, fmt.Errorf("reading the table list from %s: %s", ds.Name, e)
		}
		res.Datasource = ds
		for _, r := range records(out["tables"]) {
			t := TableInfo{
				Database:     asString(r["database"]),
				Name:         asString(r["name"]),
				Engine:       asString(r["engine"]),
				Rows:         asFloat(r["total_rows"]),
				Bytes:        asFloat(r["total_bytes"]),
				PartitionKey: asString(r["partition_key"]),
				SortingKey:   asString(r["sorting_key"]),
			}
			if m := ttlRe.FindStringSubmatch(asString(r["engine_full"])); m != nil {
				t.TTL = strings.TrimSpace(m[1])
			}
			res.Tables = append(res.Tables, t)
		}
		for _, r := range records(out["columns"]) {
			res.Columns = append(res.Columns, ColumnInfo{
				Database:       asString(r["database"]),
				Name:           asString(r["name"]),
				Type:           asString(r["type"]),
				Comment:        asString(r["comment"]),
				InSortingKey:   asBool(r["is_in_sorting_key"]),
				InPartitionKey: asBool(r["is_in_partition_key"]),
			})
		}
		return res, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("none of the ClickHouse datasources answered: %s", strings.Join(res.Unreachable, ", "))
}

func records(r SQLResult) []map[string]any {
	var out []map[string]any
	for _, f := range r.Frames {
		for i := 0; i < f.Rows; i++ {
			row := make(map[string]any, len(f.Fields))
			for _, fld := range f.Fields {
				if i < len(fld.Values) {
					row[fld.Name] = fld.Values[i]
				}
			}
			out = append(out, row)
		}
	}
	return out
}

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	case json.Number:
		return x.String()
	}
	return fmt.Sprint(v)
}

func asFloat(v any) *float64 {
	if f, ok := toFloat(v); ok {
		return &f
	}
	return nil
}

func asBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case json.Number:
		return x.String() != "0"
	case string:
		return x == "1" || strings.EqualFold(x, "true")
	}
	return false
}
