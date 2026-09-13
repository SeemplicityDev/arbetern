package commands

import (
	"context"
	"strings"
)

// readPersistentUserContext returns the stored context for the user of this
// agent: the recent conversation in channelID, older turns related to
// question and, for agents with shared memory, related turns of teammates.
func (h *GeneralHandler) readPersistentUserContext(ctx context.Context, userID, channelID, question string) UserContextView {
	if h.userContextStore == nil || h.agentID == "" || userID == "" {
		return UserContextView{}
	}
	return h.userContextStore.Context(ctx, h.agentID, userID, channelID, question)
}

// persistUserContext records a completed (question, answer) turn.
func (h *GeneralHandler) persistUserContext(ctx context.Context, userID, channelID, question, answer string) {
	if h.userContextStore == nil || h.agentID == "" || userID == "" {
		return
	}
	h.userContextStore.Append(ctx, h.agentID, userID, channelID, question, answer)
}

func (h *DebugHandler) readPersistentUserContext(ctx context.Context, userID, channelID, question string) UserContextView {
	if h.userContextStore == nil || h.agentID == "" || userID == "" {
		return UserContextView{}
	}
	return h.userContextStore.Context(ctx, h.agentID, userID, channelID, question)
}

func (h *DebugHandler) persistUserContext(ctx context.Context, userID, channelID, question, answer string) {
	if h.userContextStore == nil || h.agentID == "" || userID == "" {
		return
	}
	h.userContextStore.Append(ctx, h.agentID, userID, channelID, question, answer)
}

// userContextPrompt renders the stored context as system prompt sections.
func userContextPrompt(v UserContextView) string {
	var sb strings.Builder
	if v.Recent != "" {
		sb.WriteString("\n\nPrevious conversation with this user:\n" + v.Recent)
	}
	if v.Relevant != "" {
		sb.WriteString("\n\nRecurring topics this user has asked about previously (may hint at current intent):\n" + v.Relevant)
	}
	if v.Shared != "" {
		sb.WriteString("\n\nRelated questions teammates asked this agent recently (anonymised, background only):\n" + v.Shared)
	}
	return sb.String()
}
