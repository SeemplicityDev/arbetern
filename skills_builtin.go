package main

import (
	"sort"
	"strings"

	"github.com/justmike1/arbetern/prompts"
	"github.com/justmike1/arbetern/skills"
)

var nonSkillPromptKeys = map[string]bool{"intro": true, "classifier": true}

// builtinSkills derives read-only skills from the prompt files: one per global
// prompt block, plus one per agent-specific block or override.
func builtinSkills(agents []prompts.AgentConfig) []skills.Skill {
	global, keys, err := prompts.GlobalPrompts("")
	if err != nil {
		global, keys = nil, nil
	}
	var out []skills.Skill
	for _, k := range keys {
		text := strings.TrimSpace(global[k])
		if text == "" || nonSkillPromptKeys[k] {
			continue
		}
		out = append(out, skills.Skill{
			ID: "builtin-global-" + k, Name: skills.Humanize(k), Description: skills.FirstLine(text, 120),
			Instructions: text, Agents: []string{}, Enabled: true,
			Kind: skills.KindBuiltin, Scope: skills.ScopeGlobal, Source: "agents/prompts.yaml",
		})
	}
	for _, a := range agents {
		ks := make([]string, 0, len(a.Prompts))
		for k := range a.Prompts {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			text := strings.TrimSpace(a.Prompts[k])
			if text == "" || nonSkillPromptKeys[k] || text == strings.TrimSpace(global[k]) {
				continue
			}
			out = append(out, skills.Skill{
				ID: "builtin-" + a.ID + "-" + k, Name: skills.Humanize(k), Description: skills.FirstLine(text, 120),
				Instructions: text, Agents: []string{a.ID}, Enabled: true,
				Kind: skills.KindBuiltin, Scope: skills.ScopeAgent, Source: "agents/" + a.ID + "/prompts.yaml",
			})
		}
	}
	return out
}
