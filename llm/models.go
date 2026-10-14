package llm

import (
	"regexp"
	"strconv"
	"strings"
)

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
		providerAnthropic: "claude-haiku-5-5",
		providerAzure:     "claude-haiku-5-5",
		providerBedrock:   "global.anthropic.claude-haiku-5-5",
	},
}

// Model IDs name their family and version either as claude-<family>-<major>[-<minor>] or, before the 4 series, as claude-<major>[-<minor>]-<family>.
var (
	familyFirstRe  = regexp.MustCompile(`^claude-(fable|opus|sonnet|haiku)-(\d+)(?:-(\d{1,2}))?(?:$|[-@:\[])`)
	versionFirstRe = regexp.MustCompile(`^claude-(\d+)(?:-(\d{1,2}))?-(fable|opus|sonnet|haiku)(?:$|[-@:\[])`)
	// optionSuffixRe matches a Claude Code option such as "[1m]", kept when the ID before it is upgraded.
	optionSuffixRe = regexp.MustCompile(`\[[^\]]*\]$`)
)

// familyVersion reads a Claude model ID's family label and version, ignoring a Bedrock region prefix.
func familyVersion(id string) (ModelLabel, int, bool) {
	id = bareModelID(strings.ToLower(strings.TrimSpace(id)))
	family, major, minor := "", "", ""
	if m := familyFirstRe.FindStringSubmatch(id); m != nil {
		family, major, minor = m[1], m[2], m[3]
	} else if m := versionFirstRe.FindStringSubmatch(id); m != nil {
		family, major, minor = m[3], m[1], m[2]
	} else {
		return "", 0, false
	}
	maj, _ := strconv.Atoi(major)
	mnr, _ := strconv.Atoi(minor)
	return ModelLabel(family), maj*100 + mnr, true
}

// superseded returns the label whose current model replaces id: an older release of the same family.
func superseded(id string) ModelLabel {
	label, version, ok := familyVersion(id)
	if !ok {
		return ""
	}
	_, current, ok := familyVersion(modelIDs[label][providerAnthropic])
	if !ok || version >= current {
		return ""
	}
	return label
}

// AnthropicModel resolves a model label or ID to its Anthropic API ID; an older release of a family resolves to the family's current model.
func AnthropicModel(model string) string { return resolveModel(providerAnthropic, model) }

// KnownModel reports whether model is a label arbetern maps or an Anthropic model ID.
func KnownModel(model string) bool {
	key := strings.ToLower(strings.TrimSpace(model))
	if _, ok := modelIDs[ModelLabel(key)]; ok {
		return true
	}
	return strings.HasPrefix(key, "claude-")
}

func resolveModel(p provider, model string) string {
	name := strings.TrimSpace(model)
	key := strings.ToLower(name)
	label := ModelLabel(key)
	if _, ok := modelIDs[label]; !ok {
		native := strings.Contains(key, "anthropic.") == (p == providerBedrock)
		switch {
		case native && p != providerAnthropic:
			// A pinned Bedrock geo profile and an Azure deployment name are the operator's to choose.
			return name
		case superseded(key) != "":
			if id := modelIDs[superseded(key)][p]; id != "" {
				return id + optionSuffixRe.FindString(name)
			}
			return name
		case native:
			return name
		default:
			label = labelOf(key)
		}
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
