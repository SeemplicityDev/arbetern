// Athena read-only SQL for arbetern: the Cost and Usage Report keeps the
// resource tags Cost Explorer's aggregated dimensions drop, so per-namespace /
// per-workload cost attribution has to come from here.
//
// Nothing about the target is configured at deploy time. Every call names its
// own region, workgroup, catalog and database, which an agent discovers with
// AthenaCatalogs / AthenaSchema or takes from its prompt — the IAM policy is
// what actually bounds where a query may run and where its results land.
//
// Athena is asynchronous (start, poll, fetch); that cycle is hidden behind one
// blocking call so the tool behaves like every other query integration.
// Read-only is enforced twice: by that scoped IAM policy, and by
// readOnlyAthena rejecting any non-read statement before it is submitted.
package aws

import (
	"context"
	"fmt"
	"strings"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	atypes "github.com/aws/aws-sdk-go-v2/service/athena/types"

	"github.com/justmike1/arbetern/internal/sqlguard"
	"github.com/justmike1/arbetern/internal/text"
)

const (
	// DefaultAthenaCatalog is the built-in Glue data catalog every account has.
	DefaultAthenaCatalog = "AwsDataCatalog"

	// defaultAthenaRows / maxAthenaRows bound the rows returned: a CUR query
	// can match millions, which would bury the model's context. Aggregate in
	// SQL rather than paging through raw line items.
	defaultAthenaRows = 500
	maxAthenaRows     = 5000

	// athenaResultPage is Athena's own per-page maximum for GetQueryResults.
	athenaResultPage = 1000

	// maxAthenaTables caps a table listing so a catalog-wide browse can't
	// flood the response.
	maxAthenaTables = 200

	// athenaMaxWait bounds a single query end to end; past it the query is a
	// runaway scan and is cancelled rather than billed further.
	athenaMaxWait = 3 * time.Minute

	// Poll backoff: a small query still feels synchronous, a long one doesn't
	// hammer the API.
	athenaPollInitial = 250 * time.Millisecond
	athenaPollMax     = 2 * time.Second
)

// AthenaColumn describes one column of a result set.
type AthenaColumn struct {
	Name string `json:"name"`
	Type string `json:"type"` // Athena/Trino type, e.g. "varchar", "double", "timestamp".
}

// AthenaQueryOpts are the arguments for AthenaQuery.
type AthenaQueryOpts struct {
	SQL            string // required, read-only.
	Workgroup      string // required: bounds where the query runs and where results are written.
	Database       string // required Glue database (schema).
	Catalog        string // defaults to AwsDataCatalog.
	Region         string // defaults to the client's signing region.
	OutputLocation string // rarely needed; overrides the workgroup's own result location.
	MaxRows        int    // <=0 uses defaultAthenaRows; above maxAthenaRows is clamped.
}

// AthenaQueryResult is the flattened, LLM-friendly result of one query.
type AthenaQueryResult struct {
	QueryExecutionID string         `json:"query_execution_id"`
	Region           string         `json:"region"`
	Workgroup        string         `json:"workgroup"`
	Catalog          string         `json:"catalog"`
	Database         string         `json:"database"`
	Columns          []AthenaColumn `json:"columns"`
	Rows             [][]string     `json:"rows"` // every cell is the string form of the value; SQL NULL is rendered as "NULL".
	RowCount         int            `json:"row_count"`
	Truncated        bool           `json:"truncated"`
	DataScannedBytes int64          `json:"data_scanned_bytes"`
	ElapsedMS        int64          `json:"elapsed_ms"`
	OutputLocation   string         `json:"output_location,omitempty"`
}

// AthenaTable is one table's metadata from the Glue catalog.
type AthenaTable struct {
	Name          string         `json:"name"`
	Type          string         `json:"type,omitempty"` // EXTERNAL_TABLE, VIRTUAL_VIEW, ...
	Columns       []AthenaColumn `json:"columns,omitempty"`
	PartitionKeys []AthenaColumn `json:"partition_keys,omitempty"`
}

// AthenaCatalogsOpts selects the region whose workgroups and catalogs to list.
type AthenaCatalogsOpts struct {
	Region string // defaults to the client's signing region.
}

// AthenaCatalogsResult is the entry point for discovery: what a query may name.
type AthenaCatalogsResult struct {
	Region     string   `json:"region"`
	Workgroups []string `json:"workgroups"`
	Catalogs   []string `json:"catalogs"`
}

// AthenaSchemaOpts selects what AthenaSchema describes: one table, the tables
// of a database, or the databases of a catalog.
type AthenaSchemaOpts struct {
	Catalog   string // defaults to AwsDataCatalog.
	Database  string // empty (with no Table) lists databases instead of tables.
	Table     string // when set, returns that table's columns and partition keys.
	Search    string // optional table-name filter applied server-side when listing.
	Workgroup string // optional; scopes metadata calls to a workgroup's own permissions.
	Region    string // defaults to the client's signing region.
}

// AthenaSchemaResult carries whichever level of the catalog was asked for.
type AthenaSchemaResult struct {
	Region    string        `json:"region"`
	Catalog   string        `json:"catalog"`
	Database  string        `json:"database,omitempty"`
	Databases []string      `json:"databases,omitempty"`
	Tables    []AthenaTable `json:"tables,omitempty"`
	Truncated bool          `json:"truncated,omitempty"`
}

// AthenaQuery runs read-only SQL in the given workgroup and returns the rows
// once the query completes.
func (c *Client) AthenaQuery(ctx context.Context, opts AthenaQueryOpts) (*AthenaQueryResult, error) {
	sqlText := strings.TrimSpace(opts.SQL)
	if sqlText == "" {
		return nil, fmt.Errorf("sql is required")
	}
	if err := readOnlyAthena.Validate(sqlText); err != nil {
		return nil, err
	}
	workgroup := strings.TrimSpace(opts.Workgroup)
	if workgroup == "" {
		return nil, fmt.Errorf("workgroup is required; list the ones this role can use with aws_athena_catalogs")
	}
	database := strings.TrimSpace(opts.Database)
	if database == "" {
		return nil, fmt.Errorf("database is required; list the databases in a catalog with aws_athena_schema")
	}
	catalog := athenaCatalog(opts.Catalog)
	region := c.athenaRegion(opts.Region)
	maxRows := opts.MaxRows
	if maxRows <= 0 || maxRows > maxAthenaRows {
		maxRows = defaultAthenaRows
	}
	cl := c.athenaForRegion(region)

	in := &athena.StartQueryExecutionInput{
		QueryString: awsv2.String(sqlText),
		WorkGroup:   awsv2.String(workgroup),
		QueryExecutionContext: &atypes.QueryExecutionContext{
			Catalog:  awsv2.String(catalog),
			Database: awsv2.String(database),
		},
	}
	if loc := strings.TrimSpace(opts.OutputLocation); loc != "" {
		in.ResultConfiguration = &atypes.ResultConfiguration{OutputLocation: awsv2.String(loc)}
	}
	started, err := cl.StartQueryExecution(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("StartQueryExecution: %w", err)
	}
	id := awsv2.ToString(started.QueryExecutionId)

	exec, err := awaitAthenaQuery(ctx, cl, id)
	if err != nil {
		return nil, err
	}

	res := &AthenaQueryResult{
		QueryExecutionID: id,
		Region:           region,
		Workgroup:        workgroup,
		Catalog:          catalog,
		Database:         database,
	}
	if exec.Statistics != nil {
		if exec.Statistics.DataScannedInBytes != nil {
			res.DataScannedBytes = *exec.Statistics.DataScannedInBytes
		}
		if exec.Statistics.EngineExecutionTimeInMillis != nil {
			res.ElapsedMS = *exec.Statistics.EngineExecutionTimeInMillis
		}
	}
	if exec.ResultConfiguration != nil {
		res.OutputLocation = awsv2.ToString(exec.ResultConfiguration.OutputLocation)
	}
	if err := collectAthenaRows(ctx, cl, id, exec.StatementType, maxRows, res); err != nil {
		return nil, err
	}
	return res, nil
}

// AthenaCatalogs lists the workgroups and data catalogs visible in a region —
// the names a query has to supply, since none of them are configured.
func (c *Client) AthenaCatalogs(ctx context.Context, opts AthenaCatalogsOpts) (*AthenaCatalogsResult, error) {
	region := c.athenaRegion(opts.Region)
	cl := c.athenaForRegion(region)
	res := &AthenaCatalogsResult{Region: region}

	wgs, err := cl.ListWorkGroups(ctx, &athena.ListWorkGroupsInput{})
	if err != nil {
		return nil, fmt.Errorf("ListWorkGroups: %w", err)
	}
	for _, w := range wgs.WorkGroups {
		if n := awsv2.ToString(w.Name); n != "" {
			res.Workgroups = append(res.Workgroups, n)
		}
	}
	cats, err := cl.ListDataCatalogs(ctx, &athena.ListDataCatalogsInput{})
	if err != nil {
		return nil, fmt.Errorf("ListDataCatalogs: %w", err)
	}
	for _, cat := range cats.DataCatalogsSummary {
		if n := awsv2.ToString(cat.CatalogName); n != "" {
			res.Catalogs = append(res.Catalogs, n)
		}
	}
	return res, nil
}

// AthenaSchema describes the catalog: one table's columns and partition keys,
// the tables of a database, or — with neither named — the databases available.
func (c *Client) AthenaSchema(ctx context.Context, opts AthenaSchemaOpts) (*AthenaSchemaResult, error) {
	catalog := athenaCatalog(opts.Catalog)
	region := c.athenaRegion(opts.Region)
	cl := c.athenaForRegion(region)
	database := strings.TrimSpace(opts.Database)
	table := strings.TrimSpace(opts.Table)
	workgroup := strings.TrimSpace(opts.Workgroup)
	res := &AthenaSchemaResult{Region: region, Catalog: catalog}

	if table != "" {
		if database == "" {
			return nil, fmt.Errorf("database is required to describe table %q", table)
		}
		res.Database = database
		in := &athena.GetTableMetadataInput{
			CatalogName:  awsv2.String(catalog),
			DatabaseName: awsv2.String(database),
			TableName:    awsv2.String(table),
		}
		if workgroup != "" {
			in.WorkGroup = awsv2.String(workgroup)
		}
		out, err := cl.GetTableMetadata(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("GetTableMetadata %s.%s: %w", database, table, err)
		}
		if out.TableMetadata != nil {
			res.Tables = []AthenaTable{athenaTableFrom(*out.TableMetadata, true)}
		}
		return res, nil
	}

	if database == "" {
		in := &athena.ListDatabasesInput{CatalogName: awsv2.String(catalog)}
		if workgroup != "" {
			in.WorkGroup = awsv2.String(workgroup)
		}
		out, err := cl.ListDatabases(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("ListDatabases %s: %w", catalog, err)
		}
		for _, d := range out.DatabaseList {
			if n := awsv2.ToString(d.Name); n != "" {
				res.Databases = append(res.Databases, n)
			}
		}
		return res, nil
	}

	res.Database = database
	in := &athena.ListTableMetadataInput{
		CatalogName:  awsv2.String(catalog),
		DatabaseName: awsv2.String(database),
	}
	if workgroup != "" {
		in.WorkGroup = awsv2.String(workgroup)
	}
	if s := strings.TrimSpace(opts.Search); s != "" {
		in.Expression = awsv2.String(s)
	}
	for {
		out, err := cl.ListTableMetadata(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("ListTableMetadata %s: %w", database, err)
		}
		for _, t := range out.TableMetadataList {
			// Columns are omitted from a listing: one CUR table alone has
			// hundreds, and the caller can name the table to get them.
			res.Tables = append(res.Tables, athenaTableFrom(t, false))
			if len(res.Tables) >= maxAthenaTables {
				res.Truncated = true
				return res, nil
			}
		}
		if out.NextToken == nil || *out.NextToken == "" {
			return res, nil
		}
		in.NextToken = out.NextToken
	}
}

// awaitAthenaQuery polls until the execution finishes. On timeout it stops the
// query: an abandoned scan keeps reading S3, and keeps billing.
func awaitAthenaQuery(ctx context.Context, cl *athena.Client, id string) (*atypes.QueryExecution, error) {
	deadline := time.Now().Add(athenaMaxWait)
	delay := athenaPollInitial
	for {
		out, err := cl.GetQueryExecution(ctx, &athena.GetQueryExecutionInput{
			QueryExecutionId: awsv2.String(id),
		})
		if err != nil {
			return nil, fmt.Errorf("GetQueryExecution %s: %w", id, err)
		}
		exec := out.QueryExecution
		if exec == nil || exec.Status == nil {
			return nil, fmt.Errorf("query %s returned no status", id)
		}
		switch exec.Status.State {
		case atypes.QueryExecutionStateSucceeded:
			return exec, nil
		case atypes.QueryExecutionStateFailed, atypes.QueryExecutionStateCancelled:
			return nil, fmt.Errorf("query %s %s: %s", id, strings.ToLower(string(exec.Status.State)), athenaFailureReason(exec.Status))
		}
		if time.Now().After(deadline) {
			stopAthenaQuery(cl, id)
			return nil, fmt.Errorf("query %s did not finish within %s and was cancelled; narrow the time range or add partition filters", id, athenaMaxWait)
		}
		select {
		case <-ctx.Done():
			stopAthenaQuery(cl, id)
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if delay < athenaPollMax {
			delay *= 2
		}
	}
}

// stopAthenaQuery cancels a running execution on a fresh context, so it still
// runs when the caller's context is already done.
func stopAthenaQuery(cl *athena.Client, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = cl.StopQueryExecution(ctx, &athena.StopQueryExecutionInput{
		QueryExecutionId: awsv2.String(id),
	})
}

// collectAthenaRows pages GetQueryResults into res until maxRows is reached,
// setting Truncated when more rows were available.
func collectAthenaRows(ctx context.Context, cl *athena.Client, id string, stmt atypes.StatementType, maxRows int, res *AthenaQueryResult) error {
	in := &athena.GetQueryResultsInput{QueryExecutionId: awsv2.String(id)}
	firstPage := true
	for {
		page := maxRows - len(res.Rows)
		if page > athenaResultPage {
			page = athenaResultPage
		}
		in.MaxResults = awsv2.Int32(int32(page))
		out, err := cl.GetQueryResults(ctx, in)
		if err != nil {
			return fmt.Errorf("GetQueryResults %s: %w", id, err)
		}
		rows := []atypes.Row(nil)
		if out.ResultSet != nil {
			rows = out.ResultSet.Rows
			if len(res.Columns) == 0 && out.ResultSet.ResultSetMetadata != nil {
				for _, ci := range out.ResultSet.ResultSetMetadata.ColumnInfo {
					res.Columns = append(res.Columns, AthenaColumn{
						Name: awsv2.ToString(ci.Name),
						Type: awsv2.ToString(ci.Type),
					})
				}
			}
		}
		// For a SELECT, Athena repeats the column names as the first row of
		// the first page. Drop it only when it really is that header, so a
		// data row is never mistaken for one.
		if firstPage && stmt == atypes.StatementTypeDml && len(rows) > 0 && athenaRowIsHeader(rows[0], res.Columns) {
			rows = rows[1:]
		}
		firstPage = false
		for _, r := range rows {
			cells := make([]string, len(r.Data))
			for i, d := range r.Data {
				if d.VarCharValue == nil {
					cells[i] = "NULL"
					continue
				}
				cells[i] = *d.VarCharValue
			}
			res.Rows = append(res.Rows, cells)
		}
		if out.NextToken == nil || *out.NextToken == "" {
			break
		}
		if len(res.Rows) >= maxRows {
			res.Truncated = true
			break
		}
		in.NextToken = out.NextToken
	}
	res.RowCount = len(res.Rows)
	return nil
}

// athenaForRegion returns a cached Athena client for region, creating it on
// first use.
func (c *Client) athenaForRegion(region string) *athena.Client {
	c.athenaMu.Lock()
	defer c.athenaMu.Unlock()
	if cl, ok := c.athenaClients[region]; ok {
		return cl
	}
	cl := athena.NewFromConfig(c.cfg, func(o *athena.Options) { o.Region = region })
	c.athenaClients[region] = cl
	return cl
}

// athenaRegion resolves the region for a call, defaulting to the client's own
// signing region.
func (c *Client) athenaRegion(region string) string {
	if v := strings.TrimSpace(region); v != "" {
		return v
	}
	return c.region
}

// athenaCatalog defaults an unset catalog to the built-in Glue catalog.
func athenaCatalog(catalog string) string {
	if v := strings.TrimSpace(catalog); v != "" {
		return v
	}
	return DefaultAthenaCatalog
}

// athenaRowIsHeader reports whether row is Athena's repeated column-name row.
func athenaRowIsHeader(row atypes.Row, cols []AthenaColumn) bool {
	if len(cols) == 0 || len(row.Data) != len(cols) {
		return false
	}
	for i, d := range row.Data {
		if d.VarCharValue == nil || *d.VarCharValue != cols[i].Name {
			return false
		}
	}
	return true
}

// athenaTableFrom flattens Glue table metadata, optionally including the column
// list. Partition keys always come along: a query must filter on them to stay
// cheap.
func athenaTableFrom(t atypes.TableMetadata, withColumns bool) AthenaTable {
	out := AthenaTable{
		Name: awsv2.ToString(t.Name),
		Type: awsv2.ToString(t.TableType),
	}
	if withColumns {
		for _, col := range t.Columns {
			out.Columns = append(out.Columns, AthenaColumn{
				Name: awsv2.ToString(col.Name),
				Type: awsv2.ToString(col.Type),
			})
		}
	}
	for _, col := range t.PartitionKeys {
		out.PartitionKeys = append(out.PartitionKeys, AthenaColumn{
			Name: awsv2.ToString(col.Name),
			Type: awsv2.ToString(col.Type),
		})
	}
	return out
}

// athenaFailureReason picks the most specific message Athena offers.
func athenaFailureReason(st *atypes.QueryExecutionStatus) string {
	if st.AthenaError != nil {
		if msg := awsv2.ToString(st.AthenaError.ErrorMessage); msg != "" {
			return msg
		}
	}
	if msg := awsv2.ToString(st.StateChangeReason); msg != "" {
		return msg
	}
	return "no reason reported"
}

// readOnlyAthena is the Athena (Trino) read-only vocabulary. CTAS, INSERT and
// UNLOAD write to S3 and MSCK REPAIR rewrites partition metadata, so each is
// rejected wherever it appears.
var readOnlyAthena = sqlguard.Rules{
	Lead: map[string]bool{
		"SELECT": true, "WITH": true, "SHOW": true, "DESCRIBE": true, "DESC": true,
		"EXPLAIN": true, "VALUES": true, "TABLE": true,
	},
	Mutating: map[string]bool{
		"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true, "CREATE": true,
		"ALTER": true, "DROP": true, "TRUNCATE": true, "UNLOAD": true, "MSCK": true,
		"REPAIR": true, "GRANT": true, "REVOKE": true, "VACUUM": true, "OPTIMIZE": true,
		"ANALYZE": true, "CALL": true, "SET": true, "RESET": true, "PREPARE": true,
		"EXECUTE": true, "DEALLOCATE": true,
	},
	AllowedDesc:   "SELECT / WITH / SHOW / DESCRIBE / EXPLAIN / VALUES",
	ForbiddenDesc: "INSERT / UPDATE / DELETE / CREATE TABLE AS / UNLOAD / MSCK REPAIR / ALTER / DROP",
}

// Slack formatting

// FormatAthenaQuery renders a result set as a fixed-width table plus the bytes
// the query scanned.
func FormatAthenaQuery(r *AthenaQueryResult) string {
	if r == nil {
		return "No query result."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Athena — `%s` (workgroup %s, %s)*\n", r.Database, r.Workgroup, r.Region)
	if len(r.Rows) == 0 {
		sb.WriteString("_Query succeeded but returned no rows._\n")
	} else {
		sb.WriteString("```\n")
		sb.WriteString(athenaTableText(r.Columns, r.Rows))
		sb.WriteString("```\n")
	}
	fmt.Fprintf(&sb, "_%d rows", r.RowCount)
	if r.Truncated {
		sb.WriteString(" (truncated — aggregate in SQL or raise max_rows)")
	}
	if r.DataScannedBytes > 0 {
		fmt.Fprintf(&sb, ", %s scanned", humanSize(r.DataScannedBytes))
	}
	if r.ElapsedMS > 0 {
		fmt.Fprintf(&sb, ", %.1fs", float64(r.ElapsedMS)/1000)
	}
	sb.WriteString("_\n")
	return sb.String()
}

// FormatAthenaCatalogs renders the workgroups and catalogs available in a region.
func FormatAthenaCatalogs(r *AthenaCatalogsResult) string {
	if r == nil {
		return "No Athena workgroups or catalogs returned."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Athena in %s*\n", r.Region)
	if len(r.Workgroups) == 0 {
		sb.WriteString("_No workgroups visible to this role._\n")
	} else {
		sb.WriteString("*Workgroups*\n```\n")
		for _, w := range r.Workgroups {
			sb.WriteString(w + "\n")
		}
		sb.WriteString("```\n")
	}
	if len(r.Catalogs) > 0 {
		sb.WriteString("*Data catalogs*\n```\n")
		for _, cat := range r.Catalogs {
			sb.WriteString(cat + "\n")
		}
		sb.WriteString("```\n")
	}
	return sb.String()
}

// FormatAthenaSchema renders whichever catalog level the result carries.
func FormatAthenaSchema(r *AthenaSchemaResult) string {
	if r == nil {
		return "No catalog metadata returned."
	}
	var sb strings.Builder
	switch {
	case len(r.Databases) > 0:
		fmt.Fprintf(&sb, "*Athena databases in `%s` (%s):*\n```\n", r.Catalog, r.Region)
		for _, d := range r.Databases {
			sb.WriteString(d + "\n")
		}
		sb.WriteString("```\n")
	case len(r.Tables) == 1 && len(r.Tables[0].Columns) > 0:
		t := r.Tables[0]
		fmt.Fprintf(&sb, "*`%s.%s`* (%s)\n```\n", r.Database, t.Name, t.Type)
		for _, col := range t.Columns {
			fmt.Fprintf(&sb, "%-48s %s\n", text.Truncate(col.Name, 48), col.Type)
		}
		sb.WriteString("```\n")
		if len(t.PartitionKeys) > 0 {
			sb.WriteString("*Partition keys* (filter on these to keep scans cheap)\n```\n")
			for _, col := range t.PartitionKeys {
				fmt.Fprintf(&sb, "%-48s %s\n", text.Truncate(col.Name, 48), col.Type)
			}
			sb.WriteString("```\n")
		}
	case len(r.Tables) > 0:
		fmt.Fprintf(&sb, "*Tables in `%s.%s`:*\n```\n", r.Catalog, r.Database)
		for _, t := range r.Tables {
			fmt.Fprintf(&sb, "%-60s %s\n", text.Truncate(t.Name, 60), t.Type)
		}
		sb.WriteString("```\n")
		if r.Truncated {
			fmt.Fprintf(&sb, "_(first %d tables — narrow with a search expression)_\n", maxAthenaTables)
		}
	default:
		return "No catalog metadata returned."
	}
	return sb.String()
}

// athenaTableText lays rows out in columns sized to their content, with each
// cell capped so one long value can't stretch the table.
func athenaTableText(cols []AthenaColumn, rows [][]string) string {
	const maxCell = 40
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = len(text.Truncate(c.Name, maxCell))
	}
	cells := make([][]string, 0, len(rows))
	for _, row := range rows {
		out := make([]string, len(row))
		for i, v := range row {
			out[i] = text.Truncate(v, maxCell)
			if i < len(widths) && len(out[i]) > widths[i] {
				widths[i] = len(out[i])
			}
		}
		cells = append(cells, out)
	}
	var sb strings.Builder
	for i, c := range cols {
		fmt.Fprintf(&sb, "%-*s  ", widths[i], text.Truncate(c.Name, maxCell))
	}
	sb.WriteString("\n")
	total := 0
	for _, w := range widths {
		total += w + 2
	}
	sb.WriteString(strings.Repeat("─", total) + "\n")
	for _, row := range cells {
		for i, v := range row {
			w := maxCell
			if i < len(widths) {
				w = widths[i]
			}
			fmt.Fprintf(&sb, "%-*s  ", w, v)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
