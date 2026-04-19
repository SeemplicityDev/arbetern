package datadog

// --------------------------------------------------------------------------
// Monitor types
// --------------------------------------------------------------------------

// Monitor represents a Datadog monitor.
type Monitor struct {
	ID           int64          `json:"id"`
	Name         string         `json:"name"`
	Type         string         `json:"type"`
	Query        string         `json:"query"`
	Message      string         `json:"message"`
	Tags         []string       `json:"tags"`
	OverallState string         `json:"overall_state"`
	Creator      *MonitorAuthor `json:"creator,omitempty"`
	Created      string         `json:"created"`
	Modified     string         `json:"modified"`
	Options      interface{}    `json:"options,omitempty"`
}

// MonitorAuthor represents the creator of a monitor.
type MonitorAuthor struct {
	Name   string `json:"name"`
	Handle string `json:"handle"`
	Email  string `json:"email"`
}

// --------------------------------------------------------------------------
// Log search types
// --------------------------------------------------------------------------

// LogSearchResponse is the response from POST /api/v2/logs/events/search.
type LogSearchResponse struct {
	Data []LogEntry `json:"data"`
	Meta struct {
		Page struct {
			After string `json:"after"`
		} `json:"page"`
	} `json:"meta"`
}

// LogEntry represents a single log event.
type LogEntry struct {
	ID         string        `json:"id"`
	Type       string        `json:"type"`
	Attributes LogAttributes `json:"attributes"`
}

// LogAttributes contains the fields of a log entry.
type LogAttributes struct {
	Timestamp  string                 `json:"timestamp"`
	Host       string                 `json:"host"`
	Service    string                 `json:"service"`
	Status     string                 `json:"status"`
	Message    string                 `json:"message"`
	Tags       []string               `json:"tags"`
	Attributes map[string]interface{} `json:"attributes"`
}

// --------------------------------------------------------------------------
// Infrastructure host types
// --------------------------------------------------------------------------

// HostListResponse is the response from GET /api/v1/hosts.
type HostListResponse struct {
	HostList      []Host `json:"host_list"`
	TotalReturned int    `json:"total_returned"`
	TotalMatching int    `json:"total_matching"`
}

// Host represents a single infrastructure host.
type Host struct {
	Name         string              `json:"name"`
	ID           int64               `json:"id"`
	IsUp         bool                `json:"is_up"`
	Apps         []string            `json:"apps"`
	TagsBySource map[string][]string `json:"tags_by_source"`
	Meta         *HostMeta           `json:"meta,omitempty"`
	LastReported int64               `json:"last_reported_time"`
}

// HostMeta contains cloud/platform metadata for a host.
type HostMeta struct {
	Platform     string `json:"platform,omitempty"`
	InstanceID   string `json:"instanceID,omitempty"`
	InstanceType string `json:"instance-type,omitempty"`
}

// --------------------------------------------------------------------------
// Dashboard types
// --------------------------------------------------------------------------

// Dashboard represents a full Datadog dashboard.
type Dashboard struct {
	ID           string            `json:"id"`
	Title        string            `json:"title"`
	Description  string            `json:"description"`
	LayoutType   string            `json:"layout_type"`
	AuthorHandle string            `json:"author_handle"`
	Widgets      []DashboardWidget `json:"widgets"`
	Created      string            `json:"created_at"`
	Modified     string            `json:"modified_at"`
}

// DashboardWidget represents a widget in a dashboard.
type DashboardWidget struct {
	ID         int64                  `json:"id"`
	Definition map[string]interface{} `json:"definition,omitempty"`
}

// DashboardListResponse is the response from GET /api/v1/dashboard.
type DashboardListResponse struct {
	Dashboards []DashboardSummary `json:"dashboards"`
}

// DashboardSummary is a lightweight dashboard entry from the list endpoint.
type DashboardSummary struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	LayoutType   string `json:"layout_type"`
	AuthorHandle string `json:"author_handle"`
	Created      string `json:"created_at"`
	Modified     string `json:"modified_at"`
}

// --------------------------------------------------------------------------
// Metrics query types (v1 /api/v1/query — timeseries)
// --------------------------------------------------------------------------

// MetricsQueryResponse is the response from GET /api/v1/query.
type MetricsQueryResponse struct {
	Status   string         `json:"status"`
	ResType  string         `json:"res_type"`
	Series   []MetricSeries `json:"series"`
	FromDate int64          `json:"from_date"`
	ToDate   int64          `json:"to_date"`
	Query    string         `json:"query"`
	Message  string         `json:"message"`
	Error    string         `json:"error,omitempty"`
}

// MetricSeries is one series in a metrics query response. Each series
// corresponds to a single tag combination (a "scope" in Datadog terminology).
type MetricSeries struct {
	Metric      string   `json:"metric"`
	DisplayName string   `json:"display_name"`
	Expression  string   `json:"expression"`
	Scope       string   `json:"scope"`
	TagSet      []string `json:"tag_set"`
	Unit        []any    `json:"unit"`
	Aggr        string   `json:"aggr"`
	// PointList is an array of [timestamp_ms, value] pairs. Values can be nil
	// when Datadog has no data for that bucket.
	PointList [][]*float64 `json:"pointlist"`
	Start     int64        `json:"start"`
	End       int64        `json:"end"`
	Length    int          `json:"length"`
}
