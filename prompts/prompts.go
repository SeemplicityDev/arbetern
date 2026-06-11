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
const customConfigEnv = "CUSTOM_CONFIG_DIR"

// AgentConfig holds metadata and prompts for a single agent.
type AgentConfig struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Prompts      map[string]string `json:"prompts"`
	AllowedTeams []string          `json:"allowed_teams,omitempty"`
	ChatEnabled  bool              `json:"chat_enabled"`
}

// agentMeta is the on-disk config.yaml structure for an agent.
type agentMeta struct {
	Name         string   `yaml:"name"`
	AllowedTeams []string `yaml:"allowed_teams"`
	ChatEnabled  bool     `yaml:"chat_enabled"`
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

// loadCustomConfig reads an optional per-agent config override file from
// CUSTOM_CONFIG_DIR and returns its raw YAML bytes (nil when none exists).
// Supports two layouts:
//   - Flat file:  CUSTOM_CONFIG_DIR/<agentID>.yaml  (used by Kubernetes ConfigMap mounts)
//   - Directory:  CUSTOM_CONFIG_DIR/<agentID>/config.yaml
//
// The bytes are overlaid onto the agent's baked-in config.yaml by the caller:
// because the override is a full config file, only the keys present in it take
// effect, and any field added to the agent config in the future is overridable
// automatically — no code change required.
func loadCustomConfig(agentID string) []byte {
	customDir := os.Getenv(customConfigEnv)
	if customDir == "" {
		return nil
	}
	// Try flat file first (ConfigMap mount: <dir>/<agentID>.yaml).
	path := filepath.Join(customDir, agentID+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		// Fall back to directory layout: <dir>/<agentID>/config.yaml.
		path = filepath.Join(customDir, agentID, agentConfigFile)
		data, err = os.ReadFile(path)
		if err != nil {
			return nil // no custom config for this agent
		}
	}
	return data
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
		var meta agentMeta

		// Load the baked-in config.yaml, then overlay any deployment override
		// from CUSTOM_CONFIG_DIR. Both decode into the same struct: YAML only
		// assigns keys that are present, so a (possibly partial) override file
		// changes just the fields it sets and leaves everything else at the
		// baked-in value. Any field added to agentMeta in the future is
		// overridable automatically — the override is a full config file.
		configPath := filepath.Join(agentsDir, entry.Name(), agentConfigFile)
		if cfgData, err := os.ReadFile(configPath); err == nil {
			_ = yaml.Unmarshal(cfgData, &meta)
		}
		if override := loadCustomConfig(entry.Name()); override != nil {
			_ = yaml.Unmarshal(override, &meta)
		}

		if meta.Name != "" {
			displayName = meta.Name
		}

		agents = append(agents, AgentConfig{
			ID:           name,
			Name:         displayName,
			Prompts:      merged,
			AllowedTeams: meta.AllowedTeams,
			ChatEnabled:  meta.ChatEnabled,
		})
	}

	return agents, nil
}
