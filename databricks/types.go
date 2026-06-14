package databricks

// This file holds the request/response shapes for the Databricks SQL
// Statement Execution API (POST/GET /api/2.0/sql/statements) plus the
// flattened public types arbetern hands back to the tool layer.

// --------------------------------------------------------------------------
// Public types (returned to callers)
// --------------------------------------------------------------------------

// Column describes one column of a query result set.
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"` // SQL type text (e.g. "DATE", "DECIMAL(38,2)", "STRING").
}

// QueryParam is a named SQL parameter substituted for a `:name` marker in the
// statement. Value is always passed as a string; Type (optional) lets
// Databricks cast it (e.g. "DATE", "INT"). An empty Type means "interpret as
// STRING". To bind NULL, leave Value empty and set Type to the target type.
type QueryParam struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Type  string `json:"type,omitempty"`
}

// QueryResult is the flattened, LLM-friendly result of a SQL statement.
type QueryResult struct {
	Columns     []Column   `json:"columns"`
	Rows        [][]string `json:"rows"` // each cell is the string form of the value; SQL NULL is rendered as "NULL".
	RowCount    int        `json:"row_count"`
	Truncated   bool       `json:"truncated"`
	WarehouseID string     `json:"warehouse_id"`
	StatementID string     `json:"statement_id"`
}

// --------------------------------------------------------------------------
// Wire types (Statement Execution API)
// --------------------------------------------------------------------------

// statementResponse is returned by POST /api/2.0/sql/statements and
// GET /api/2.0/sql/statements/{id}. Fields beyond statement_id + status may
// be absent depending on the execution state.
type statementResponse struct {
	StatementID string          `json:"statement_id"`
	Status      statementStatus `json:"status"`
	Manifest    *resultManifest `json:"manifest,omitempty"`
	Result      *resultData     `json:"result,omitempty"`
}

// statementStatus carries the execution state and, on failure, the error.
// State is one of PENDING, RUNNING, SUCCEEDED, FAILED, CANCELED, CLOSED.
type statementStatus struct {
	State string          `json:"state"`
	Error *statementError `json:"error,omitempty"`
}

type statementError struct {
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
}

// resultManifest provides schema + metadata for the result set.
type resultManifest struct {
	Format        string       `json:"format"`
	Schema        resultSchema `json:"schema"`
	TotalRowCount int64        `json:"total_row_count"`
	Truncated     bool         `json:"truncated"`
}

type resultSchema struct {
	ColumnCount int         `json:"column_count"`
	Columns     []schemaCol `json:"columns"`
}

type schemaCol struct {
	Name     string `json:"name"`
	TypeName string `json:"type_name"`
	TypeText string `json:"type_text"`
	Position int    `json:"position"`
}

// resultData is one chunk of inline (JSON_ARRAY) result data. Each value in
// DataArray is either the string representation of a cell or nil for SQL NULL.
// When NextChunkInternalLink/NextChunkIndex are set there are further chunks
// to fetch.
type resultData struct {
	ChunkIndex            int         `json:"chunk_index"`
	RowOffset             int64       `json:"row_offset"`
	RowCount              int64       `json:"row_count"`
	DataArray             [][]*string `json:"data_array"`
	NextChunkIndex        *int        `json:"next_chunk_index,omitempty"`
	NextChunkInternalLink string      `json:"next_chunk_internal_link,omitempty"`
}
