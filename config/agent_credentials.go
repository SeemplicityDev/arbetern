package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
)

// AgentCredentialsEnv names the directory where per-agent credential override
// files are mounted. Each agent gets its own subdirectory; inside it every
// file is treated as a single override value whose filename matches one of
// the kebab-case secret keys used in the Helm chart's `secretValues` map
// (e.g. `sf-consumer-key`, `chorus-api-token`, `dd-api-key-us`).
//
// Layout produced by the chart:
//
//	$AGENT_CREDENTIALS_DIR/
//	  <agent-id>/
//	    sf-consumer-key
//	    sf-consumer-secret
//	    ...
//
// This mirrors how Kubernetes projects Secret keys when a Secret is mounted
// as a volume.
const AgentCredentialsEnv = "AGENT_CREDENTIALS_DIR"

// credentialTag names the struct tag on Credentials fields that maps each
// field to a kebab-case secret key. The tag value is the single source of
// truth for the key name — this file contains no hardcoded secret names;
// adding a new chart credential is just adding `cred:"new-key"` on the
// field that owns the value.
const credentialTag = "cred"

// LoadAgentOverrides returns the credential overrides for a single agent.
// Keys are the kebab-case Helm secret keys; values are the trimmed file
// contents. Returns nil when no override directory is mounted for the agent
// (the common case — most agents share the global credentials).
//
// Errors reading individual files are intentionally swallowed: a partial
// override is still useful, and the caller falls back to the global value
// for any key that is missing or unreadable.
func LoadAgentOverrides(agentID string) map[string]string {
	root := strings.TrimSpace(os.Getenv(AgentCredentialsEnv))
	if root == "" || agentID == "" {
		return nil
	}
	dir := filepath.Join(root, agentID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	overrides := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Kubernetes Secret projections create symlinks via a hidden
		// `..data` directory; skip the dotfiles to avoid surfacing them
		// as bogus credential keys.
		if strings.HasPrefix(name, "..") || strings.HasPrefix(name, ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		overrides[name] = strings.TrimRight(string(data), "\r\n")
	}
	if len(overrides) == 0 {
		return nil
	}
	return overrides
}

// ForAgent returns a copy of the receiver with any per-agent credential
// overrides applied on top. Fields with no override fall through to the
// global value. When no overrides are mounted the receiver is returned
// unchanged so callers can pointer-compare to detect that the global
// client can be reused as-is.
func (c *Config) ForAgent(agentID string) *Config {
	overrides := LoadAgentOverrides(agentID)
	if len(overrides) == 0 {
		return c
	}
	cp := *c
	idx := credentialFieldIndex()
	v := reflect.ValueOf(&cp.Credentials).Elem()
	for key, value := range overrides {
		fieldIdx, ok := idx[key]
		if !ok {
			continue // unknown key — chart added something the app doesn't model yet
		}
		v.Field(fieldIdx).SetString(value)
	}
	return &cp
}

// credentialFieldIndex returns a {kebab-secret-key → struct-field-index}
// map built once via reflection from the `cred:"..."` tags on the
// Credentials struct. Computed lazily and cached forever; the Credentials
// type is package-local and static for the lifetime of the process.
var credentialFieldIndex = sync.OnceValue(func() map[string]int {
	t := reflect.TypeOf(Credentials{})
	idx := make(map[string]int, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get(credentialTag)
		if tag == "" {
			continue
		}
		idx[tag] = i
	}
	return idx
})
