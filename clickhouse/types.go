package clickhouse

// usageCostResponse is the envelope returned by GET …/usageCost.
type usageCostResponse struct {
	Status    int             `json:"status"`
	RequestID string          `json:"requestId"`
	Result    usageCostResult `json:"result"`
}

type usageCostResult struct {
	GrandTotalCHC float64           `json:"grandTotalCHC"`
	Costs         []UsageCostRecord `json:"costs"`
}

// errorResponse is the API's error envelope ({status,error,requestId}).
type errorResponse struct {
	Status    int    `json:"status"`
	Error     string `json:"error"`
	RequestID string `json:"requestId"`
}

// UsageCostRecord is one daily, per-entity usage-cost record. A record belongs
// to a service, a data warehouse, or a ClickPipe (see EntityType).
type UsageCostRecord struct {
	DataWarehouseID string           `json:"dataWarehouseId"`
	ServiceID       string           `json:"serviceId"`  // null for dataWarehouse entities.
	Date            string           `json:"date"`       // ISO-8601 date (UTC).
	EntityType      string           `json:"entityType"` // datawarehouse | service | clickpipe
	EntityID        string           `json:"entityId"`
	EntityName      string           `json:"entityName"`
	Metrics         UsageCostMetrics `json:"metrics"`
	TotalCHC        float64          `json:"totalCHC"`
	Locked          bool             `json:"locked"` // immutable once true; unlocked records may still change.
}

// UsageCostMetrics breaks a record's cost (in ClickHouse Credits) down by
// dimension. Which fields are populated depends on the entity type.
type UsageCostMetrics struct {
	StorageCHC                      float64 `json:"storageCHC"`
	BackupCHC                       float64 `json:"backupCHC"`
	ComputeCHC                      float64 `json:"computeCHC"`
	DataTransferCHC                 float64 `json:"dataTransferCHC"`
	InitialLoadCHC                  float64 `json:"initialLoadCHC"`
	PublicDataTransferCHC           float64 `json:"publicDataTransferCHC"`
	InterRegionTier1DataTransferCHC float64 `json:"interRegionTier1DataTransferCHC"`
	InterRegionTier2DataTransferCHC float64 `json:"interRegionTier2DataTransferCHC"`
	InterRegionTier3DataTransferCHC float64 `json:"interRegionTier3DataTransferCHC"`
	InterRegionTier4DataTransferCHC float64 `json:"interRegionTier4DataTransferCHC"`
}

// UsageCostReport is the public, app-facing result of GetUsageCost: the grand
// total plus the daily per-entity records for the queried window.
type UsageCostReport struct {
	OrganizationID string
	FromDate       string
	ToDate         string
	Filters        []string
	GrandTotalCHC  float64
	Records        []UsageCostRecord
}
