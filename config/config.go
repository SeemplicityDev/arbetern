package config

import (
	_ "embed"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// extensions.json is generated during the Docker build from github-linguist.
// When building locally without the file, the embed yields an empty JSON array
// and extension-based code-intent detection is silently skipped.
//
//go:embed extensions.json
var ExtensionsRaw string

const (
	defaultPort             = "8080"
	defaultThreadSessionTTL = 8 * time.Minute
	// defaultChatRetention is how long a UI chat conversation is kept after
	// its last activity before the background sweeper deletes it. One week by
	// default; override with CHAT_RETENTION (Go duration).
	defaultChatRetention = 7 * 24 * time.Hour
	// defaultMaxToolRounds bounds the LLM tool-loop per request. Multi-step
	// scheduled workflows (e.g. an auto-fix tick that walks 10 Jira tickets,
	// fetches several SKILL.md files, opens PRs, and posts comments) can
	// easily issue 100+ tool calls in a single tick. The previous ceiling of
	// 50 caused those workflows to auto-disable after 3 consecutive
	// max-rounds failures, even when each individual tool call succeeded.
	// 200 leaves headroom for the worst observed legitimate flow without
	// becoming a license to loop forever.
	defaultMaxToolRounds = 200
)

// Credentials holds every value that ships to the app as a Kubernetes
// Secret entry (and the corresponding env var locally). Each field is
// tagged with the kebab-case key the Helm chart uses in `secretValues` /
// `customCredentials`, which is also the single source of truth for the
// per-agent override system in agent_credentials.go.
//
// Splitting these off from the rest of Config keeps the "what can be
// overridden per agent" surface area obvious — anything in this struct
// can be overridden, nothing outside it can.
type Credentials struct {
	SlackBotToken      string `cred:"slack-bot-token"`
	SlackSigningSecret string `cred:"slack-signing-secret"`
	SlackAppToken      string `cred:"slack-app-token"`
	GitHubToken        string `cred:"github-token"`
	AzureEndpoint      string `cred:"azure-openai-endpoint"`
	AzureAPIKey        string `cred:"azure-api-key"`
	// BedrockAPIKey is an AWS Bedrock API key (bearer token). When set, the
	// Bedrock backend authenticates with it instead of resolving SigV4
	// credentials. Sourced from AWS_BEARER_TOKEN_BEDROCK. Requires
	// BedrockRegion to select Bedrock.
	BedrockAPIKey         string `cred:"bedrock-api-key"`
	AtlassianURL          string `cred:"atlassian-url"`
	AtlassianEmail        string `cred:"atlassian-email"`
	AtlassianAPIToken     string `cred:"atlassian-api-token"`
	AtlassianClientID     string `cred:"atlassian-client-id"`
	AtlassianClientSecret string `cred:"atlassian-client-secret"`
	JiraProject           string `cred:"jira-project"`
	NVDAPIKey             string `cred:"nvd-api-key"`
	SFConsumerKey         string `cred:"sf-consumer-key"`
	SFConsumerSecret      string `cred:"sf-consumer-secret"`
	SFLoginURL            string `cred:"sf-login-url"` // defaults to "https://login.salesforce.com"
	ChorusAPIToken        string `cred:"chorus-api-token"`
	ChorusBaseURL         string `cred:"chorus-base-url"` // defaults to "https://chorus.ai"
	DDAPIKeyUS            string `cred:"dd-api-key-us"`   // Datadog US (datadoghq.com)
	DDAppKeyUS            string `cred:"dd-app-key-us"`
	DDAPIKeyEU            string `cred:"dd-api-key-eu"` // Datadog EU (datadoghq.eu)
	DDAppKeyEU            string `cred:"dd-app-key-eu"`

	// Azure Cost Management service-principal — distinct from the
	// AzureEndpoint/AzureAPIKey fields above which configure Azure OpenAI
	// for LLM access. Cost Management uses a client-credentials flow
	// against AAD.
	AzureTenantID          string `cred:"azure-tenant-id"`
	AzureClientID          string `cred:"azure-client-id"`
	AzureClientSecret      string `cred:"azure-client-secret"`
	AzureManagementGroupID string `cred:"azure-management-group-id"` // Optional. Defaults to tenant root MG (= tenant ID) for tenant-wide cost reporting.
	AzureBillingAccountID  string `cred:"azure-billing-account-id"`  // Optional. EA enrollment number. When set, queries hit billing-account scope (preferred for EA tenants) and AzureManagementGroupID is ignored.

	// Databricks SQL warehouse — OAuth 2.0 machine-to-machine (service
	// principal) against the workspace token endpoint. The host is the
	// workspace URL; the warehouse ID selects the compute that runs queries.
	DatabricksHost         string `cred:"databricks-host"`          // Workspace URL, e.g. "https://dbc-1234.cloud.databricks.com".
	DatabricksClientID     string `cred:"databricks-client-id"`     // Service-principal OAuth client ID.
	DatabricksClientSecret string `cred:"databricks-client-secret"` // Service-principal OAuth client secret.
	DatabricksWarehouseID  string `cred:"databricks-warehouse-id"`  // SQL warehouse ID that executes statements.

	// ClickHouse Cloud billing — HTTP Basic auth against the Cloud API
	// (https://api.clickhouse.cloud). The key ID/secret are generated in the
	// ClickHouse Cloud console; the organization ID selects which org the
	// usage-cost report covers.
	ClickHouseKeyID          string `cred:"clickhouse-key-id"`          // Cloud API key ID (HTTP Basic username).
	ClickHouseKeySecret      string `cred:"clickhouse-key-secret"`      // Cloud API key secret (HTTP Basic password).
	ClickHouseOrganizationID string `cred:"clickhouse-organization-id"` // Organization ID the report covers.

	// ClickHouse SQL query interface — HTTP Basic auth against a service's
	// HTTPS endpoint. A separate surface from the billing API above: it runs
	// read-only SQL against the databases/tables. Use a SELECT/SHOW-only user.
	ClickHouseQueryEndpoint string `cred:"clickhouse-query-endpoint"` // Service HTTPS endpoint, e.g. "https://abc123.us-east-1.aws.clickhouse.cloud:8443".
	ClickHouseQueryUser     string `cred:"clickhouse-query-user"`     // Read-only database username (HTTP Basic username).
	ClickHouseQueryPassword string `cred:"clickhouse-query-password"` // Database password (HTTP Basic password).

	// Freshworks suite (read-only). Each product authenticates differently and
	// is configured independently — a product with missing credentials is
	// simply not advertised. Freshdesk uses HTTP Basic (API key as username);
	// Freshchat uses a Bearer JWT; the CRM uses a `Token token=<key>` header.
	FreshdeskDomain     string `cred:"freshdesk-domain"`       // Freshdesk host, e.g. "acme.freshdesk.com".
	FreshdeskAPIKey     string `cred:"freshdesk-api-key"`      // Freshdesk API key (HTTP Basic username).
	FreshchatURL        string `cred:"freshchat-url"`          // Freshchat API base incl. /v2, e.g. "https://acme-123.freshchat.com/v2".
	FreshchatAPIToken   string `cred:"freshchat-api-token"`    // Freshchat API token (Bearer JWT).
	FreshworksCRMDomain string `cred:"freshworks-crm-domain"`  // Freshworks CRM host, e.g. "acme.myfreshworks.com".
	FreshworksCRMAPIKey string `cred:"freshworks-crm-api-key"` // Freshworks CRM API key.
}

// Config bundles every runtime setting the app reads on startup. Credential
// values live on the embedded Credentials struct so they can be promoted
// (`cfg.SlackBotToken`) while also being addressable as a unit
// (`cfg.Credentials`) for the per-agent override path.
type Config struct {
	Credentials

	GeneralModel string // Required model/deployment for general queries.
	CodeModel    string // Model/deployment for code-generation tasks (PRs, modify_file). Falls back to GeneralModel when unset.
	// CodeModelExplicit is true when CODE_MODEL was set explicitly (before the
	// fall-back to GeneralModel is applied), so callers can surface a distinct
	// code model only when the operator actually configured one.
	CodeModelExplicit bool
	Port              string
	UIAllowedCIDRs    string
	AppURL            string
	ThreadSessionTTL  time.Duration
	MaxToolRounds     int

	// HeadroomURL is the base URL of a Headroom compression proxy (empty =
	// disabled); set from HEADROOM_PROXY_URL. See llm.Client.compressMessages.
	HeadroomURL string

	// HeadroomTimeout bounds a single /v1/compress round-trip before the app
	// gives up and sends the conversation uncompressed (fail-open). Set from
	// HEADROOM_COMPRESS_TIMEOUT; zero uses the llm package default.
	HeadroomTimeout time.Duration

	// AWSRegion controls where Cost Explorer SigV4 calls are signed. Empty
	// falls back to the aws package default (us-east-1, where the CE
	// endpoint lives). Credentials are resolved via the standard AWS SDK
	// chain — env vars (AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY), a
	// shared profile (AWS_PROFILE), or EKS IRSA
	// (AWS_WEB_IDENTITY_TOKEN_FILE + AWS_ROLE_ARN) all work.
	AWSRegion  string
	AWSEnabled bool // set true when AWS credentials appear present (enables the cost-explorer tools).

	// BedrockRegion selects Amazon Bedrock as the LLM backend when set (e.g.
	// "us-east-1"). It is deliberately separate from AWSRegion (which signs
	// Cost Explorer calls): Bedrock model / inference-profile availability is
	// region-specific, and setting it is the explicit opt-in that distinguishes
	// "use Bedrock for inference" from "AWS creds happen to be present for the
	// cost tools". Credentials resolve through the same AWS SDK chain.
	BedrockRegion string

	// Sovereign-cloud overrides for Azure Cost Management. Default to the
	// public-cloud endpoints when unset. Not per-agent overridable — these
	// are deployment-wide.
	AzureAuthorityHost  string
	AzureManagementHost string

	DashboardsDir string // Directory where dashboard JSON snapshots are persisted.
	WorkflowsDir  string // Directory where workflow JSON snapshots are persisted.
	ChatDir       string // Directory where per-agent chat transcripts are persisted.
	BillingDir    string // Directory where LLM usage/cost aggregates are persisted.

	// ChatRetention is how long a UI chat conversation is kept after its last
	// activity before it is deleted by the background sweeper. Applies to all
	// agents. Defaults to one week; override with CHAT_RETENTION.
	ChatRetention time.Duration

	// GitOps sync: when WorkflowsGitOpsRepo is set, arbetern reconciles
	// workflow JSON descriptors from a remote GitHub repo into the local
	// registry. See docs/WORKFLOWS_GITOPS.md.
	WorkflowsGitOpsOwner    string
	WorkflowsGitOpsRepo     string        // required to enable
	WorkflowsGitOpsBranch   string        // empty == repo default branch
	WorkflowsGitOpsBasePath string        // defaults to "arbetern/workflows"
	WorkflowsGitOpsInterval time.Duration // defaults to 5m, minimum 30s
	WorkflowsGitOpsPrune    bool

	// GitOps sync: same model as workflows, but for dashboards. The base
	// path defaults to "arbetern/dashboards".
	DashboardsGitOpsOwner    string
	DashboardsGitOpsRepo     string
	DashboardsGitOpsBranch   string
	DashboardsGitOpsBasePath string
	DashboardsGitOpsInterval time.Duration
	DashboardsGitOpsPrune    bool
}

// UseAzure returns true when Azure OpenAI credentials are configured.
func (c *Config) UseAzure() bool {
	return c.AzureEndpoint != "" && c.AzureAPIKey != ""
}

// UseBedrock returns true when Amazon Bedrock is selected as the LLM backend
// (BEDROCK_REGION is set). Bedrock takes precedence over Azure OpenAI and
// GitHub Models when more than one is configured.
func (c *Config) UseBedrock() bool {
	return c.BedrockRegion != ""
}

// AtlassianConfigured returns true when Atlassian credentials are present.
// Supports both Basic Auth (email + API token) and OAuth 2.0 (client ID + secret).
func (c *Config) AtlassianConfigured() bool {
	if c.AtlassianURL == "" {
		return false
	}
	return (c.AtlassianEmail != "" && c.AtlassianAPIToken != "") || (c.AtlassianClientID != "" && c.AtlassianClientSecret != "")
}

// AtlassianUseOAuth returns true when OAuth 2.0 client credentials are configured.
func (c *Config) AtlassianUseOAuth() bool {
	return c.AtlassianClientID != "" && c.AtlassianClientSecret != ""
}

// SalesforceConfigured returns true when Salesforce consumer credentials are present.
func (c *Config) SalesforceConfigured() bool {
	return c.SFConsumerKey != "" && c.SFConsumerSecret != ""
}

// ChorusConfigured returns true when a Chorus API token is present.
func (c *Config) ChorusConfigured() bool {
	return c.ChorusAPIToken != ""
}

// DatadogConfigured returns true when at least one Datadog site (US or EU) has credentials.
func (c *Config) DatadogConfigured() bool {
	return c.DatadogUSConfigured() || c.DatadogEUConfigured()
}

// DatadogUSConfigured returns true when US Datadog credentials are present.
func (c *Config) DatadogUSConfigured() bool {
	return c.DDAPIKeyUS != "" && c.DDAppKeyUS != ""
}

// DatadogEUConfigured returns true when EU Datadog credentials are present.
func (c *Config) DatadogEUConfigured() bool {
	return c.DDAPIKeyEU != "" && c.DDAppKeyEU != ""
}

// AWSConfigured returns true when at least one of the canonical AWS
// credential sources appears to be set. This is a hint to the integration
// wiring only; the aws package still does a real STS-style probe on
// NewClient, so a "yes" here does not guarantee working credentials.
func (c *Config) AWSConfigured() bool {
	return c.AWSEnabled
}

// AzureCostConfigured returns true when the Azure service-principal
// values required for Cost Management calls (tenant, client, secret) are
// present. AzureManagementGroupID is optional and defaults to the tenant
// root management group; sovereign-cloud host overrides are also optional.
func (c *Config) AzureCostConfigured() bool {
	return c.AzureTenantID != "" && c.AzureClientID != "" &&
		c.AzureClientSecret != ""
}

// DatabricksConfigured returns true when the Databricks workspace host plus
// the service-principal OAuth credentials and a SQL warehouse ID are all
// present. The first real query is still the authoritative health check.
func (c *Config) DatabricksConfigured() bool {
	return c.DatabricksHost != "" && c.DatabricksClientID != "" &&
		c.DatabricksClientSecret != "" && c.DatabricksWarehouseID != ""
}

// ClickHouseConfigured returns true when the ClickHouse Cloud API key ID, key
// secret and organization ID are all present. The first real request is the
// authoritative health check.
func (c *Config) ClickHouseConfigured() bool {
	return c.ClickHouseKeyID != "" && c.ClickHouseKeySecret != "" &&
		c.ClickHouseOrganizationID != ""
}

// ClickHouseQueryConfigured returns true when the ClickHouse SQL query
// endpoint and database username are present (the password may be empty for a
// passwordless user). The first real query is the authoritative health check.
func (c *Config) ClickHouseQueryConfigured() bool {
	return c.ClickHouseQueryEndpoint != "" && c.ClickHouseQueryUser != ""
}

// FreshdeskConfigured returns true when the Freshdesk domain and API key are
// both present.
func (c *Config) FreshdeskConfigured() bool {
	return c.FreshdeskDomain != "" && c.FreshdeskAPIKey != ""
}

// FreshchatConfigured returns true when the Freshchat URL and token are both
// present.
func (c *Config) FreshchatConfigured() bool {
	return c.FreshchatURL != "" && c.FreshchatAPIToken != ""
}

// FreshworksCRMConfigured returns true when the Freshworks CRM domain and API
// key are both present.
func (c *Config) FreshworksCRMConfigured() bool {
	return c.FreshworksCRMDomain != "" && c.FreshworksCRMAPIKey != ""
}

// FreshworksConfigured returns true when at least one Freshworks product
// (Freshdesk, Freshchat, or CRM) has its credentials present.
func (c *Config) FreshworksConfigured() bool {
	return c.FreshdeskConfigured() || c.FreshchatConfigured() || c.FreshworksCRMConfigured()
}

func Load() (*Config, error) {
	cfg := &Config{
		Credentials: Credentials{
			SlackBotToken:          os.Getenv("SLACK_BOT_TOKEN"),
			SlackSigningSecret:     os.Getenv("SLACK_SIGNING_SECRET"),
			SlackAppToken:          os.Getenv("SLACK_APP_TOKEN"),
			GitHubToken:            os.Getenv("GITHUB_TOKEN"),
			AzureEndpoint:          os.Getenv("AZURE_OPEN_AI_ENDPOINT"),
			AzureAPIKey:            os.Getenv("AZURE_API_KEY"),
			BedrockAPIKey:          os.Getenv("AWS_BEARER_TOKEN_BEDROCK"),
			AtlassianURL:           os.Getenv("ATLASSIAN_URL"),
			AtlassianEmail:         os.Getenv("ATLASSIAN_EMAIL"),
			AtlassianAPIToken:      os.Getenv("ATLASSIAN_API_TOKEN"),
			AtlassianClientID:      os.Getenv("ATLASSIAN_CLIENT_ID"),
			AtlassianClientSecret:  os.Getenv("ATLASSIAN_CLIENT_SECRET"),
			JiraProject:            os.Getenv("JIRA_PROJECT"),
			NVDAPIKey:              os.Getenv("NVD_API_KEY"),
			SFConsumerKey:          os.Getenv("SF_CONSUMER_KEY"),
			SFConsumerSecret:       os.Getenv("SF_CONSUMER_SECRET"),
			SFLoginURL:             os.Getenv("SF_LOGIN_URL"),
			ChorusAPIToken:         os.Getenv("CHORUS_API_TOKEN"),
			ChorusBaseURL:          os.Getenv("CHORUS_BASE_URL"),
			DDAPIKeyUS:             os.Getenv("DD_API_KEY_US"),
			DDAppKeyUS:             os.Getenv("DD_APP_KEY_US"),
			DDAPIKeyEU:             os.Getenv("DD_API_KEY_EU"),
			DDAppKeyEU:             os.Getenv("DD_APP_KEY_EU"),
			AzureTenantID:          os.Getenv("AZURE_TENANT_ID"),
			AzureClientID:          os.Getenv("AZURE_CLIENT_ID"),
			AzureClientSecret:      os.Getenv("AZURE_CLIENT_SECRET"),
			AzureManagementGroupID: os.Getenv("AZURE_MANAGEMENT_GROUP_ID"),
			AzureBillingAccountID:  os.Getenv("AZURE_BILLING_ACCOUNT_ID"),
			DatabricksHost:         os.Getenv("DATABRICKS_HOST"),

			ClickHouseQueryEndpoint: os.Getenv("CLICKHOUSE_QUERY_ENDPOINT"),
			ClickHouseQueryUser:     os.Getenv("CLICKHOUSE_QUERY_USER"),
			ClickHouseQueryPassword: os.Getenv("CLICKHOUSE_QUERY_PASSWORD"),
			DatabricksClientID:      os.Getenv("DATABRICKS_CLIENT_ID"),
			DatabricksClientSecret:  os.Getenv("DATABRICKS_CLIENT_SECRET"),
			DatabricksWarehouseID:   os.Getenv("DATABRICKS_WAREHOUSE_ID"),

			ClickHouseKeyID:          os.Getenv("CLICKHOUSE_KEY_ID"),
			ClickHouseKeySecret:      os.Getenv("CLICKHOUSE_KEY_SECRET"),
			ClickHouseOrganizationID: os.Getenv("CLICKHOUSE_ORGANIZATION_ID"),

			FreshdeskDomain:     os.Getenv("FRESHDESK_DOMAIN"),
			FreshdeskAPIKey:     os.Getenv("FRESHDESK_API_KEY"),
			FreshchatURL:        os.Getenv("FRESHCHAT_URL"),
			FreshchatAPIToken:   os.Getenv("FRESHCHAT_API_TOKEN"),
			FreshworksCRMDomain: os.Getenv("FRESHWORKS_CRM_DOMAIN"),
			FreshworksCRMAPIKey: os.Getenv("FRESHWORKS_CRM_API_KEY"),
		},

		GeneralModel:        os.Getenv("GENERAL_MODEL"),
		CodeModel:           os.Getenv("CODE_MODEL"),
		Port:                os.Getenv("PORT"),
		UIAllowedCIDRs:      os.Getenv("UI_ALLOWED_CIDRS"),
		AppURL:              os.Getenv("APP_URL"),
		HeadroomURL:         strings.TrimRight(strings.TrimSpace(os.Getenv("HEADROOM_PROXY_URL")), "/"),
		AWSRegion:           os.Getenv("AWS_REGION"),
		BedrockRegion:       strings.TrimSpace(os.Getenv("BEDROCK_REGION")),
		AzureAuthorityHost:  os.Getenv("AZURE_AUTHORITY_HOST"),
		AzureManagementHost: os.Getenv("AZURE_MANAGEMENT_HOST"),
		DashboardsDir:       os.Getenv("DASHBOARDS_DIR"),
		WorkflowsDir:        os.Getenv("WORKFLOWS_DIR"),
		ChatDir:             os.Getenv("CHAT_DIR"),
		BillingDir:          os.Getenv("BILLING_DIR"),

		WorkflowsGitOpsOwner:    os.Getenv("WORKFLOWS_GITOPS_OWNER"),
		WorkflowsGitOpsRepo:     os.Getenv("WORKFLOWS_GITOPS_REPO"),
		WorkflowsGitOpsBranch:   os.Getenv("WORKFLOWS_GITOPS_BRANCH"),
		WorkflowsGitOpsBasePath: os.Getenv("WORKFLOWS_GITOPS_BASE_PATH"),
		WorkflowsGitOpsPrune:    strings.EqualFold(strings.TrimSpace(os.Getenv("WORKFLOWS_GITOPS_PRUNE")), "true"),

		DashboardsGitOpsOwner:    os.Getenv("DASHBOARDS_GITOPS_OWNER"),
		DashboardsGitOpsRepo:     os.Getenv("DASHBOARDS_GITOPS_REPO"),
		DashboardsGitOpsBranch:   os.Getenv("DASHBOARDS_GITOPS_BRANCH"),
		DashboardsGitOpsBasePath: os.Getenv("DASHBOARDS_GITOPS_BASE_PATH"),
		DashboardsGitOpsPrune:    strings.EqualFold(strings.TrimSpace(os.Getenv("DASHBOARDS_GITOPS_PRUNE")), "true"),
	}
	if s := strings.TrimSpace(os.Getenv("WORKFLOWS_GITOPS_INTERVAL")); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			cfg.WorkflowsGitOpsInterval = d
		} else {
			return nil, fmt.Errorf("invalid WORKFLOWS_GITOPS_INTERVAL %q: %w", s, err)
		}
	}
	if s := strings.TrimSpace(os.Getenv("DASHBOARDS_GITOPS_INTERVAL")); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			cfg.DashboardsGitOpsInterval = d
		} else {
			return nil, fmt.Errorf("invalid DASHBOARDS_GITOPS_INTERVAL %q: %w", s, err)
		}
	}
	// AWS credentials can arrive via several standard channels. We set
	// AWSEnabled when any of them is present; the real credential probe
	// happens in aws.NewClient.
	cfg.AWSEnabled = os.Getenv("AWS_ACCESS_KEY_ID") != "" ||
		os.Getenv("AWS_PROFILE") != "" ||
		os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE") != "" ||
		os.Getenv("AWS_ROLE_ARN") != "" ||
		os.Getenv("AWS_SHARED_CREDENTIALS_FILE") != ""

	if cfg.SlackBotToken == "" {
		return nil, fmt.Errorf("SLACK_BOT_TOKEN is required")
	}
	if cfg.SlackSigningSecret == "" {
		return nil, fmt.Errorf("SLACK_SIGNING_SECRET is required")
	}

	// A GitHub token, Azure credentials, or a Bedrock region is required for
	// LLM access.
	if cfg.GitHubToken == "" && !cfg.UseAzure() && !cfg.UseBedrock() {
		return nil, fmt.Errorf("GITHUB_TOKEN is required (or set AZURE_OPEN_AI_ENDPOINT and AZURE_API_KEY, or BEDROCK_REGION)")
	}

	// GENERAL_MODEL is required for every backend — there is no default model,
	// so an unset value never silently ships a wrong provider's model ID.
	if cfg.GeneralModel == "" {
		return nil, fmt.Errorf("GENERAL_MODEL is required")
	}
	if cfg.Port == "" {
		cfg.Port = defaultPort
	}

	// CODE_MODEL is optional and falls back to the general model when unset.
	// Record whether it was set explicitly before applying the fall-back.
	cfg.CodeModelExplicit = cfg.CodeModel != ""
	if cfg.CodeModel == "" {
		cfg.CodeModel = cfg.GeneralModel
	}

	if mtrStr := os.Getenv("MAX_TOOL_ROUNDS"); mtrStr != "" {
		if n, err := strconv.Atoi(mtrStr); err == nil && n > 0 {
			cfg.MaxToolRounds = n
		} else {
			return nil, fmt.Errorf("invalid MAX_TOOL_ROUNDS %q: must be a positive integer", mtrStr)
		}
	} else {
		cfg.MaxToolRounds = defaultMaxToolRounds
	}

	if ttlStr := os.Getenv("THREAD_SESSION_TTL"); ttlStr != "" {
		if d, err := time.ParseDuration(ttlStr); err == nil && d > 0 {
			cfg.ThreadSessionTTL = d
		} else {
			return nil, fmt.Errorf("invalid THREAD_SESSION_TTL %q: must be a positive Go duration (e.g. 3m, 5m30s)", ttlStr)
		}
	} else {
		cfg.ThreadSessionTTL = defaultThreadSessionTTL
	}

	if hcStr := os.Getenv("HEADROOM_COMPRESS_TIMEOUT"); hcStr != "" {
		if d, err := time.ParseDuration(hcStr); err == nil && d > 0 {
			cfg.HeadroomTimeout = d
		} else {
			return nil, fmt.Errorf("invalid HEADROOM_COMPRESS_TIMEOUT %q: must be a positive Go duration (e.g. 90s, 2m)", hcStr)
		}
	}

	if retStr := os.Getenv("CHAT_RETENTION"); retStr != "" {
		if d, err := time.ParseDuration(retStr); err == nil && d > 0 {
			cfg.ChatRetention = d
		} else {
			return nil, fmt.Errorf("invalid CHAT_RETENTION %q: must be a positive Go duration (e.g. 168h, 720h)", retStr)
		}
	} else {
		cfg.ChatRetention = defaultChatRetention
	}

	return cfg, nil
}
