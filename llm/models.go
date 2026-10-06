package llm

import "strings"

// ModelLabel is a provider-neutral model name, accepted wherever a model ID is.
type ModelLabel string

const (
	ModelFable  ModelLabel = "fable"
	ModelOpus   ModelLabel = "opus"
	ModelSonnet ModelLabel = "sonnet"
	ModelHaiku  ModelLabel = "haiku"
)

type provider string

const (
	providerAnthropic provider = "anthropic"
	providerBedrock   provider = "bedrock"
	providerAzure     provider = "azure"
	providerGitHub    provider = "github"
)

var modelIDs = map[ModelLabel]map[provider]string{
	ModelFable: {
		providerAnthropic: "claude-fable-5-1",
		providerAzure:     "claude-fable-5-1",
	},
	ModelOpus: {
		providerAnthropic: "claude-opus-5-5",
		providerAzure:     "claude-opus-5-5",
		providerBedrock:   "global.anthropic.claude-opus-5-5",
	},
	ModelSonnet: {
		providerAnthropic: "claude-sonnet-5-5",
		providerAzure:     "claude-sonnet-5-5",
		providerBedrock:   "global.anthropic.claude-sonnet-5-5",
	},
	ModelHaiku: {
		providerAnthropic: "claude-haiku-4-5",
		providerAzure:     "claude-haiku-4-5",
		providerBedrock:   "global.anthropic.claude-haiku-4-5-20251001-v1:0",
	},
}

func resolveModel(p provider, model string) string {
	name := strings.TrimSpace(model)
	key := strings.ToLower(name)
	label := ModelLabel(key)
	if _, ok := modelIDs[label]; !ok {
		// An ID already in p's form passes through, so a pinned Bedrock geo profile is kept.
		if strings.Contains(key, "anthropic.") == (p == providerBedrock) {
			return name
		}
		label = labelOf(key)
	}
	if id := modelIDs[label][p]; id != "" {
		return id
	}
	return name
}

func labelOf(id string) ModelLabel {
	id = bareModelID(id)
	for label, ids := range modelIDs {
		for _, known := range ids {
			if bareModelID(known) == id {
				return label
			}
		}
	}
	return ""
}

func bareModelID(id string) string {
	if _, after, ok := strings.Cut(id, "anthropic."); ok {
		return after
	}
	return id
}
