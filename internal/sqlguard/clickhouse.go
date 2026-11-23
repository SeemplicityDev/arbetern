package sqlguard

import (
	"fmt"
	"regexp"
)

// SYSTEM is not forbidden so system.* stays readable; a SYSTEM command can only lead a statement, which Lead refuses.
var clickHouse = Rules{
	Lead: map[string]bool{
		"SELECT": true, "WITH": true, "SHOW": true, "DESCRIBE": true, "DESC": true, "EXISTS": true, "EXPLAIN": true,
	},
	Mutating: map[string]bool{
		"INSERT": true, "ALTER": true, "CREATE": true, "DROP": true, "DETACH": true, "ATTACH": true, "RENAME": true,
		"TRUNCATE": true, "OPTIMIZE": true, "GRANT": true, "REVOKE": true, "KILL": true, "UPDATE": true, "DELETE": true,
		"MOVE": true, "FREEZE": true, "UNFREEZE": true, "SET": true, "WATCH": true, "BACKUP": true, "RESTORE": true,
	},
	AllowedDesc:   "SELECT / WITH / SHOW / DESCRIBE / EXISTS / EXPLAIN",
	ForbiddenDesc: "INSERT / ALTER / CREATE / DROP / DELETE / SET",
}

var externalTableFuncRe = regexp.MustCompile(`(?i)\b(url|urlCluster|s3|s3Cluster|gcs|hdfs|hdfsCluster|azureBlobStorage|azureBlobStorageCluster|remote|remoteSecure|mysql|postgresql|jdbc|odbc|mongodb|redis|sqlite|file|fileCluster|executable|input|deltaLake|hudi|iceberg)\s*\(`)

// ValidateClickHouse rejects ClickHouse SQL that is not clearly read-only or that calls a table function reaching outside the database.
func ValidateClickHouse(sql string) error {
	if err := clickHouse.Validate(sql); err != nil {
		return err
	}
	if m := externalTableFuncRe.FindStringSubmatch(sql); m != nil {
		return fmt.Errorf("the %s() table function is not allowed: queries may only read tables in the database", m[1])
	}
	return nil
}
