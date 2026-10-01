package commands

// IntegrationDatadog is the Datadog integration name accepted by AgentCanUseIntegration.
const IntegrationDatadog = integrationDatadog

// AgentCanUseIntegration reports whether agentID may use the named integration.
func AgentCanUseIntegration(agentID, integration string) bool {
	return agentCanUseIntegration(agentID, integration)
}
