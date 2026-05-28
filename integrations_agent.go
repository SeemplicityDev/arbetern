package main

import (
	"context"
	"log"
	"time"

	"github.com/justmike1/arbetern/atlassian"
	"github.com/justmike1/arbetern/aws"
	"github.com/justmike1/arbetern/azure"
	"github.com/justmike1/arbetern/chorus"
	"github.com/justmike1/arbetern/config"
	"github.com/justmike1/arbetern/datadog"
	"github.com/justmike1/arbetern/nvd"
	"github.com/justmike1/arbetern/salesforce"
)

// agentIntegrationClients bundles the third-party integration clients that
// the per-agent router needs. When `customCredentials.<agent>` is set in the
// Helm chart the app rebuilds only the clients whose credentials actually
// changed for that agent and reuses the shared globals for everything else.
type agentIntegrationClients struct {
	jira    *atlassian.Client
	sf      *salesforce.Client
	chorus  *chorus.Client
	datadog *datadog.MultiClient
	aws     *aws.Client
	azure   *azure.Client
	nvd     *nvd.Client
}

// buildAgentScopedClients returns a set of integration clients tailored for a
// single agent. When `agentCfg == globalCfg` (no overrides mounted for the
// agent) every field falls through to the corresponding global client so
// there is no extra connection setup.
//
// Slack, GitHub and LLM clients are intentionally not rebuilt here:
//   - Slack tokens are a deployment-wide identity; overriding them per agent
//     would require a second bot user, which is out of scope.
//   - The GitHub and LLM clients are shared infrastructure that the router
//     does not currently re-parameterise per agent.
//
// Any future per-agent integration only needs a new branch below.
func buildAgentScopedClients(
	globalCfg *config.Config,
	agentCfg *config.Config,
	agentID string,
	global agentIntegrationClients,
) agentIntegrationClients {
	out := global
	if agentCfg == globalCfg {
		return out
	}

	if agentCfg.AtlassianURL != globalCfg.AtlassianURL ||
		agentCfg.AtlassianEmail != globalCfg.AtlassianEmail ||
		agentCfg.AtlassianAPIToken != globalCfg.AtlassianAPIToken ||
		agentCfg.AtlassianClientID != globalCfg.AtlassianClientID ||
		agentCfg.AtlassianClientSecret != globalCfg.AtlassianClientSecret ||
		agentCfg.JiraProject != globalCfg.JiraProject {
		switch {
		case !agentCfg.AtlassianConfigured():
			out.jira = nil
		case agentCfg.AtlassianUseOAuth():
			out.jira = atlassian.NewOAuthClient(agentCfg.AtlassianURL, agentCfg.AtlassianClientID, agentCfg.AtlassianClientSecret, agentCfg.JiraProject)
			log.Printf("Atlassian override for agent %q (OAuth): %s (default project: %s)", agentID, agentCfg.AtlassianURL, agentCfg.JiraProject)
		default:
			out.jira = atlassian.NewClient(agentCfg.AtlassianURL, agentCfg.AtlassianEmail, agentCfg.AtlassianAPIToken, agentCfg.JiraProject)
			log.Printf("Atlassian override for agent %q (Basic Auth): %s (default project: %s)", agentID, agentCfg.AtlassianURL, agentCfg.JiraProject)
		}
	}

	if agentCfg.NVDAPIKey != globalCfg.NVDAPIKey {
		out.nvd = nvd.NewClient(agentCfg.NVDAPIKey)
		log.Printf("NVD override for agent %q (api key %s)", agentID, maskedPresent(agentCfg.NVDAPIKey))
	}

	if agentCfg.SFConsumerKey != globalCfg.SFConsumerKey ||
		agentCfg.SFConsumerSecret != globalCfg.SFConsumerSecret ||
		agentCfg.SFLoginURL != globalCfg.SFLoginURL {
		if agentCfg.SalesforceConfigured() {
			out.sf = salesforce.NewClient(agentCfg.SFConsumerKey, agentCfg.SFConsumerSecret, agentCfg.SFLoginURL)
			log.Printf("Salesforce override for agent %q", agentID)
		} else {
			out.sf = nil
		}
	}

	if agentCfg.ChorusAPIToken != globalCfg.ChorusAPIToken ||
		agentCfg.ChorusBaseURL != globalCfg.ChorusBaseURL {
		if agentCfg.ChorusConfigured() {
			out.chorus = chorus.NewClient(agentCfg.ChorusAPIToken, agentCfg.ChorusBaseURL)
			log.Printf("Chorus override for agent %q", agentID)
		} else {
			out.chorus = nil
		}
	}

	if agentCfg.DDAPIKeyUS != globalCfg.DDAPIKeyUS ||
		agentCfg.DDAppKeyUS != globalCfg.DDAppKeyUS ||
		agentCfg.DDAPIKeyEU != globalCfg.DDAPIKeyEU ||
		agentCfg.DDAppKeyEU != globalCfg.DDAppKeyEU {
		if agentCfg.DatadogConfigured() {
			out.datadog = datadog.NewMultiClient(agentCfg.DDAPIKeyUS, agentCfg.DDAppKeyUS, agentCfg.DDAPIKeyEU, agentCfg.DDAppKeyEU)
			log.Printf("Datadog override for agent %q (sites: %s)", agentID, out.datadog.Sites())
		} else {
			out.datadog = nil
		}
	}

	if agentCfg.AzureTenantID != globalCfg.AzureTenantID ||
		agentCfg.AzureClientID != globalCfg.AzureClientID ||
		agentCfg.AzureClientSecret != globalCfg.AzureClientSecret ||
		agentCfg.AzureManagementGroupID != globalCfg.AzureManagementGroupID ||
		agentCfg.AzureBillingAccountID != globalCfg.AzureBillingAccountID {
		if agentCfg.AzureCostConfigured() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			c, err := azure.NewClient(ctx, agentCfg.AzureTenantID, agentCfg.AzureClientID, agentCfg.AzureClientSecret, agentCfg.AzureManagementGroupID, agentCfg.AzureBillingAccountID, agentCfg.AzureAuthorityHost, agentCfg.AzureManagementHost)
			cancel()
			if err != nil {
				log.Printf("Azure override for agent %q failed (falling back to global): %v", agentID, err)
			} else {
				out.azure = c
				log.Printf("Azure override for agent %q (tenant: %s)", agentID, c.TenantID())
			}
		} else {
			out.azure = nil
		}
	}

	// AWS uses ambient credentials (SDK chain) rather than discrete fields in
	// the Config struct, so a per-agent override of `aws-access-key-id` /
	// `aws-secret-access-key` does NOT currently propagate to the AWS client
	// here — the SDK reads the process-level env vars instead. Document the
	// limitation in the chart values so deployers don't expect it to work.

	return out
}

// maskedPresent returns a short presence indicator for log lines so we don't
// echo override secrets even at INFO level.
func maskedPresent(v string) string {
	if v == "" {
		return "unset"
	}
	return "set"
}
