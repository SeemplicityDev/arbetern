package config

import (
	_ "embed"
	"fmt"
	"os"
	"strconv"
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
	defaultMaxToolRounds    = 50
)

type Config struct {
	SlackBotToken         string
	SlackSigningSecret    string
	GitHubToken           string
	GeneralModel          string // Default model/deployment for general queries.
	CodeModel             string // Separate model/deployment for code-generation tasks (PRs, modify_file).
	AzureEndpoint         string
	AzureAPIKey           string
	Port                  string
	UIAllowedCIDRs        string
	AtlassianURL          string
	AtlassianEmail        string
	AtlassianAPIToken     string
	JiraProject           string
	AtlassianClientID     string
	AtlassianClientSecret string
	AppURL                string
	SlackAppToken         string
	ThreadSessionTTL      time.Duration
	MaxToolRounds         int
	NVDAPIKey             string
	SFConsumerKey         string
	SFConsumerSecret      string
	SFLoginURL            string // defaults to "https://login.salesforce.com"
	ChorusAPIToken        string
	ChorusBaseURL         string // defaults to "https://chorus.ai"
	DDAPIKeyUS            string // Datadog US (datadoghq.com)
	DDAppKeyUS            string
	DDAPIKeyEU            string // Datadog EU (datadoghq.eu)
	DDAppKeyEU            string
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

func Load() (*Config, error) {
	cfg := &Config{
		SlackBotToken:         os.Getenv("SLACK_BOT_TOKEN"),
		SlackSigningSecret:    os.Getenv("SLACK_SIGNING_SECRET"),
		GitHubToken:           os.Getenv("GITHUB_TOKEN"),
		GeneralModel:          os.Getenv("GENERAL_MODEL"),
		CodeModel:             os.Getenv("CODE_MODEL"),
		AzureEndpoint:         os.Getenv("AZURE_OPEN_AI_ENDPOINT"),
		AzureAPIKey:           os.Getenv("AZURE_API_KEY"),
		Port:                  os.Getenv("PORT"),
		UIAllowedCIDRs:        os.Getenv("UI_ALLOWED_CIDRS"),
		AtlassianURL:          os.Getenv("ATLASSIAN_URL"),
		AtlassianEmail:        os.Getenv("ATLASSIAN_EMAIL"),
		AtlassianAPIToken:     os.Getenv("ATLASSIAN_API_TOKEN"),
		JiraProject:           os.Getenv("JIRA_PROJECT"),
		AtlassianClientID:     os.Getenv("ATLASSIAN_CLIENT_ID"),
		AtlassianClientSecret: os.Getenv("ATLASSIAN_CLIENT_SECRET"),
		AppURL:                os.Getenv("APP_URL"),
		SlackAppToken:         os.Getenv("SLACK_APP_TOKEN"),
		NVDAPIKey:             os.Getenv("NVD_API_KEY"),
		SFConsumerKey:         os.Getenv("SF_CONSUMER_KEY"),
		SFConsumerSecret:      os.Getenv("SF_CONSUMER_SECRET"),
		SFLoginURL:            os.Getenv("SF_LOGIN_URL"),
		ChorusAPIToken:        os.Getenv("CHORUS_API_TOKEN"),
		ChorusBaseURL:         os.Getenv("CHORUS_BASE_URL"),
		DDAPIKeyUS:            os.Getenv("DD_API_KEY_US"),
		DDAppKeyUS:            os.Getenv("DD_APP_KEY_US"),
		DDAPIKeyEU:            os.Getenv("DD_API_KEY_EU"),
		DDAppKeyEU:            os.Getenv("DD_APP_KEY_EU"),
	}

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
