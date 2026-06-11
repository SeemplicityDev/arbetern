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
	defaultModel            = "openai/gpt-4o"
	defaultAzureModel       = "gpt-4o"
	defaultThreadSessionTTL = 3 * time.Minute
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
	SlackBotToken         string `cred:"slack-bot-token"`
	SlackSigningSecret    string `cred:"slack-signing-secret"`
	SlackAppToken         string `cred:"slack-app-token"`
	GitHubToken           string `cred:"github-token"`
	AzureEndpoint         string `cred:"azure-openai-endpoint"`
	AzureAPIKey           string `cred:"azure-api-key"`
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
}

// Config bundles every runtime setting the app reads on startup. Credential
// values live on the embedded Credentials struct so they can be promoted
// (`cfg.SlackBotToken`) while also being addressable as a unit
// (`cfg.Credentials`) for the per-agent override path.
type Config struct {
	Credentials

	GeneralModel     string // Default model/deployment for general queries.
	CodeModel        string // Separate model/deployment for code-generation tasks (PRs, modify_file).
	Port             string
	UIAllowedCIDRs   string
	AppURL           string
	ThreadSessionTTL time.Duration
	MaxToolRounds    int

	// AWSRegion controls where Cost Explorer SigV4 calls are signed. Empty
	// falls back to the aws package default (us-east-1, where the CE
	// endpoint lives). Credentials are resolved via the standard AWS SDK
	// chain — env vars (AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY), a
	// shared profile (AWS_PROFILE), or EKS IRSA
	// (AWS_WEB_IDENTITY_TOKEN_FILE + AWS_ROLE_ARN) all work.
	AWSRegion  string
	AWSEnabled bool // set true when AWS credentials appear present (enables the cost-explorer tools).

	// Sovereign-cloud overrides for Azure Cost Management. Default to the
	// public-cloud endpoints when unset. Not per-agent overridable — these
	// are deployment-wide.
	AzureAuthorityHost  string
	AzureManagementHost string

	DashboardsDir string // Directory where dashboard JSON snapshots are persisted.
	WorkflowsDir  string // Directory where workflow JSON snapshots are persisted.
	ChatDir       string // Directory where per-agent chat transcripts are persisted.

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

func Load() (*Config, error) {
	cfg := &Config{
		Credentials: Credentials{
			SlackBotToken:          os.Getenv("SLACK_BOT_TOKEN"),
			SlackSigningSecret:     os.Getenv("SLACK_SIGNING_SECRET"),
			SlackAppToken:          os.Getenv("SLACK_APP_TOKEN"),
			GitHubToken:            os.Getenv("GITHUB_TOKEN"),
			AzureEndpoint:          os.Getenv("AZURE_OPEN_AI_ENDPOINT"),
			AzureAPIKey:            os.Getenv("AZURE_API_KEY"),
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
		},

		GeneralModel:        os.Getenv("GENERAL_MODEL"),
		CodeModel:           os.Getenv("CODE_MODEL"),
		Port:                os.Getenv("PORT"),
		UIAllowedCIDRs:      os.Getenv("UI_ALLOWED_CIDRS"),
		AppURL:              os.Getenv("APP_URL"),
		AWSRegion:           os.Getenv("AWS_REGION"),
		AzureAuthorityHost:  os.Getenv("AZURE_AUTHORITY_HOST"),
		AzureManagementHost: os.Getenv("AZURE_MANAGEMENT_HOST"),
		DashboardsDir:       os.Getenv("DASHBOARDS_DIR"),
		WorkflowsDir:        os.Getenv("WORKFLOWS_DIR"),
		ChatDir:             os.Getenv("CHAT_DIR"),

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

	// Either GitHub token or Azure credentials are required for LLM access.
	if cfg.GitHubToken == "" && !cfg.UseAzure() {
		return nil, fmt.Errorf("GITHUB_TOKEN is required (or set AZURE_OPEN_AI_ENDPOINT and AZURE_API_KEY)")
	}

	if cfg.GeneralModel == "" {
		if cfg.UseAzure() {
			cfg.GeneralModel = defaultAzureModel
		} else {
			cfg.GeneralModel = defaultModel
		}
	}
	if cfg.Port == "" {
		cfg.Port = defaultPort
	}

	// CODE_MODEL defaults to the general model when not explicitly set.
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

	return cfg, nil
}
