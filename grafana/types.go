package grafana

import "encoding/json"

// SearchHit is one dashboard or folder from /api/search.
type SearchHit struct {
	UID         string   `json:"uid"`
	Title       string   `json:"title"`
	Type        string   `json:"type"`
	URL         string   `json:"url"`
	Tags        []string `json:"tags"`
	FolderUID   string   `json:"folderUid"`
	FolderTitle string   `json:"folderTitle"`
	Description string   `json:"description"`
}

// Datasource is one datasource the service account can see.
type Datasource struct {
	UID       string         `json:"uid"`
	Name      string         `json:"name"`
	Type      string         `json:"type"`
	TypeName  string         `json:"typeName"`
	URL       string         `json:"url"`
	Database  string         `json:"database"`
	IsDefault bool           `json:"isDefault"`
	ReadOnly  bool           `json:"readOnly"`
	JSONData  map[string]any `json:"jsonData"`
}

type frontendSettings struct {
	DefaultDatasource string `json:"defaultDatasource"`
	Datasources       map[string]struct {
		ID        int64          `json:"id"`
		UID       string         `json:"uid"`
		Name      string         `json:"name"`
		Type      string         `json:"type"`
		URL       string         `json:"url"`
		IsDefault bool           `json:"isDefault"`
		JSONData  map[string]any `json:"jsonData"`
		Meta      struct {
			Name string `json:"name"`
		} `json:"meta"`
	} `json:"datasources"`
}

type dashboardEnvelope struct {
	Dashboard map[string]any `json:"dashboard"`
	Meta      struct {
		URL         string `json:"url"`
		FolderUID   string `json:"folderUid"`
		FolderTitle string `json:"folderTitle"`
		Updated     string `json:"updated"`
		UpdatedBy   string `json:"updatedBy"`
		Version     int    `json:"version"`
		Provisioned bool   `json:"provisioned"`
	} `json:"meta"`
}

// Annotation is one entry of /api/annotations.
type Annotation struct {
	ID           int64    `json:"id"`
	DashboardUID string   `json:"dashboardUID"`
	PanelID      int64    `json:"panelId"`
	Login        string   `json:"login"`
	Text         string   `json:"text"`
	Tags         []string `json:"tags"`
	Time         int64    `json:"time"`
	TimeEnd      int64    `json:"timeEnd"`
	AlertName    string   `json:"alertName"`
	NewState     string   `json:"newState"`
}

type dsQueryResponse struct {
	Results map[string]dsResult `json:"results"`
}

type dsResult struct {
	Status int       `json:"status"`
	Error  string    `json:"error"`
	Frames []dsFrame `json:"frames"`
}

// dsFrame is Grafana's JSON data frame: a schema plus column-major values.
type dsFrame struct {
	Schema struct {
		Name   string    `json:"name"`
		RefID  string    `json:"refId"`
		Fields []dsField `json:"fields"`
		Meta   struct {
			ExecutedQueryString string `json:"executedQueryString"`
			Notices             []struct {
				Severity string `json:"severity"`
				Text     string `json:"text"`
			} `json:"notices"`
		} `json:"meta"`
	} `json:"schema"`
	Data struct {
		Values [][]json.RawMessage `json:"values"`
	} `json:"data"`
}

type dsField struct {
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	Labels   map[string]string `json:"labels"`
	TypeInfo struct {
		Frame string `json:"frame"`
	} `json:"typeInfo"`
	Config struct {
		DisplayNameFromDS string `json:"displayNameFromDS"`
	} `json:"config"`
}
