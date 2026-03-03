package prompts

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const defaultAgentsDir = "agents"
const globalPromptsFile = "prompts.yaml"
const agentConfigFile = "config.yaml"
const customPromptsEnv = "CUSTOM_PROMPTS_DIR"
const rbacOverrideEnv = "AGENT_RBAC_DIR"

// AgentConfig holds metadata and prompts for a single agent.
type AgentConfig struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Prompts      map[string]string `json:"prompts"`
	AllowedTeams []string          `json:"allowed_teams,omitempty"`
}

// agentMeta is the on-disk config.yaml structure for an agent.
type agentMeta struct {
	Name         string   `yaml:"name"`
	AllowedTeams []string `yaml:"allowed_teams"`
}

// AgentPrompts holds a per-agent prompt store with Get/MustGet methods.
type AgentPrompts struct {
	agentID    string
	store      map[string]string
	globalKeys []string // ordered keys from agents/prompts.yaml
}

// loadGlobalPrompts reads the global prompts.yaml from the agents root directory.
// It returns the parsed key-value map and the keys in their original YAML order
// so that SystemPrompt can assemble them deterministically.
func loadGlobalPrompts(agentsDir string) (map[string]string, []string, error) {
	path := filepath.Join(agentsDir, globalPromptsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil // no global prompts — not an error
		}
		return nil, nil, fmt.Errorf("failed to read global prompts: %w", err)
	}

	// Decode via yaml.Node to preserve key order (Go maps are unordered).
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("failed to parse global prompts: %w", err)
	}

	parsed := make(map[string]string)
	var keys []string
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		mapping := doc.Content[0]
		if mapping.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(mapping.Content); i += 2 {
				k := mapping.Content[i].Value
				v := mapping.Content[i+1].Value
				keys = append(keys, k)
				parsed[k] = v
			}
		}
	}
	return parsed, keys, nil
}

// loadRBACOverride reads optional RBAC overrides for an agent from AGENT_RBAC_DIR.
// When present, the override REPLACES (not merges) the config.yaml allowed_teams.
// Supports two layouts:
//   - Flat file:  AGENT_RBAC_DIR/<agentID>.yaml
//   - Directory:  AGENT_RBAC_DIR/<agentID>/config.yaml
func loadRBACOverride(agentID string) []string {
	rbacDir := os.Getenv(rbacOverrideEnv)
	if rbacDir == "" {
		return nil
	}
	// Try flat file first (ConfigMap mount: <dir>/<agentID>.yaml).
	path := filepath.Join(rbacDir, agentID+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		// Fall back to directory layout: <dir>/<agentID>/config.yaml.
		path = filepath.Join(rbacDir, agentID, "config.yaml")
		data, err = os.ReadFile(path)
		if err != nil {
			return nil // no RBAC override for this agent
		}
	}
	var override struct {
		AllowedTeams []string `yaml:"allowed_teams"`
	}
	if err := yaml.Unmarshal(data, &override); err != nil {
		return nil
	}
	return override.AllowedTeams
}

// loadCustomPrompts reads optional custom prompts for an agent from CUSTOM_PROMPTS_DIR.
// Supports two layouts:
//   - Flat file:  CUSTOM_PROMPTS_DIR/<agentID>.yaml  (used by Kubernetes ConfigMap mounts)
//   - Directory:  CUSTOM_PROMPTS_DIR/<agentID>/prompts.yaml
//
// Custom prompts are APPENDED to existing prompt keys (not overridden). New keys are added as-is.
// This allows deployers to inject org-specific context without modifying the built-in prompts.
func loadCustomPrompts(agentID string) map[string]string {
	customDir := os.Getenv(customPromptsEnv)
	if customDir == "" {
		return nil
	}
	// Try flat file first (ConfigMap mount: <dir>/<agentID>.yaml).
	path := filepath.Join(customDir, agentID+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		// Fall back to directory layout: <dir>/<agentID>/prompts.yaml.
		path = filepath.Join(customDir, agentID, "prompts.yaml")
		data, err = os.ReadFile(path)
		if err != nil {
			return nil // no custom prompts for this agent
		}
	}
	parsed := make(map[string]string)
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return nil
	}
	return parsed
}

// appendCustomPrompts merges custom prompts into an existing prompt map.
// For keys that already exist, the custom value is APPENDED (with a double newline separator).
// For new keys, the custom value is added directly.
func appendCustomPrompts(merged map[string]string, custom map[string]string) {
	for k, v := range custom {
		if existing, ok := merged[k]; ok {
			merged[k] = existing + "\n\n" + v
		} else {
			merged[k] = v
		}
	}
}

// LoadAgent reads the prompts.yaml for the given agent and returns an AgentPrompts.
// Global prompts from agents/prompts.yaml are loaded first; agent-specific prompts override them.
func LoadAgent(agentID string) (*AgentPrompts, error) {
	agentsDir := os.Getenv("AGENTS_DIR")
	if agentsDir == "" {
		agentsDir = defaultAgentsDir
	}

	// Start with global prompts as the base.
	merged, globalKeys, err := loadGlobalPrompts(agentsDir)
	if err != nil {
		return nil, err
	}
	if merged == nil {
		merged = make(map[string]string)
	}

	// Layer agent-specific prompts on top (overrides globals).
	path := filepath.Join(agentsDir, agentID, "prompts.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read prompts for agent %s: %w", agentID, err)
	}
	parsed := make(map[string]string)
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("failed to parse prompts for agent %s: %w", agentID, err)
	}
	for k, v := range parsed {
		merged[k] = v
	}

	// Append custom prompts from CUSTOM_PROMPTS_DIR (org-specific context from ConfigMap).
	if custom := loadCustomPrompts(agentID); custom != nil {
		appendCustomPrompts(merged, custom)
	}

	return &AgentPrompts{agentID: agentID, store: merged, globalKeys: globalKeys}, nil
}

// Get returns the prompt for the given key, or empty string if not found.
func (ap *AgentPrompts) Get(key string) string {
	if ap == nil || ap.store == nil {
		return ""
	}
	return ap.store[key]
}

// MustGet returns the prompt for the given key or panics if not found.
func (ap *AgentPrompts) MustGet(key string) string {
	val := ap.Get(key)
	if val == "" {
		panic(fmt.Sprintf("prompt %q not found for agent %s", key, ap.agentID))
	}
	return val
}

// GetAll returns a copy of all prompts in this agent store.
func (ap *AgentPrompts) GetAll() map[string]string {
	if ap == nil || ap.store == nil {
		return nil
	}
	cp := make(map[string]string, len(ap.store))
	for k, v := range ap.store {
		cp[k] = v
	}
	return cp
}

// SystemPrompt builds a system prompt by joining all global keys (in their
// original YAML order) followed by the handler-specific key, separated by
// double newlines. Adding a new key to agents/prompts.yaml automatically
// includes it — no code changes required.
func (ap *AgentPrompts) SystemPrompt(specificKey string) string {
	parts := make([]string, 0, len(ap.globalKeys)+1)
	for _, k := range ap.globalKeys {
		if v := ap.Get(k); v != "" {
			parts = append(parts, v)
		}
	}
	if v := ap.Get(specificKey); v != "" {
		parts = append(parts, v)
	}
	return strings.Join(parts, "\n\n")
}

// ID returns the agent identifier.
func (ap *AgentPrompts) ID() string {
	return ap.agentID
}

// DiscoverAgents scans the agents directory and returns all agent configs.
// Each subdirectory under agentsDir is treated as an agent, with a prompts.yaml inside.
// Global prompts from agents/prompts.yaml are merged as a base for each agent.
// An optional config.yaml in the agent directory can set a custom display name.
func DiscoverAgents(agentsDir string) ([]AgentConfig, error) {
	if agentsDir == "" {
		agentsDir = os.Getenv("AGENTS_DIR")
	}
	if agentsDir == "" {
		agentsDir = defaultAgentsDir
	}

	globalPrompts, _, err := loadGlobalPrompts(agentsDir)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read agents directory %s: %w", agentsDir, err)
	}

	var agents []AgentConfig
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		promptsPath := filepath.Join(agentsDir, entry.Name(), "prompts.yaml")
		data, err := os.ReadFile(promptsPath)
		if err != nil {
			continue // skip dirs without prompts.yaml
		}

		parsed := make(map[string]string)
		if err := yaml.Unmarshal(data, &parsed); err != nil {
			continue
		}

		// Merge: global prompts as base, agent-specific on top.
		merged := make(map[string]string, len(globalPrompts)+len(parsed))
		for k, v := range globalPrompts {
			merged[k] = v
		}
		for k, v := range parsed {
			merged[k] = v
		}

		// Append custom prompts from CUSTOM_PROMPTS_DIR (org-specific context from ConfigMap).
		if custom := loadCustomPrompts(entry.Name()); custom != nil {
			appendCustomPrompts(merged, custom)
		}

		name := entry.Name()
		displayName := strings.ToUpper(name[:1]) + name[1:]
		var allowedTeams []string

		// Check for config.yaml with a custom display name and RBAC settings.
		configPath := filepath.Join(agentsDir, entry.Name(), agentConfigFile)
		if cfgData, err := os.ReadFile(configPath); err == nil {
			var meta agentMeta
			if err := yaml.Unmarshal(cfgData, &meta); err == nil {
				if meta.Name != "" {
					displayName = meta.Name
				}
				allowedTeams = meta.AllowedTeams
			}
		}

		// Apply RBAC override from AGENT_RBAC_DIR if present (replaces config.yaml value).
		if rbacOverride := loadRBACOverride(entry.Name()); rbacOverride != nil {
			allowedTeams = rbacOverride
		}

		agents = append(agents, AgentConfig{
			ID:           name,
			Name:         displayName,
			Prompts:      merged,
			AllowedTeams: allowedTeams,
		})
	}

	return agents, nil
}
