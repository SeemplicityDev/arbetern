package commands

import "context"

// readPersistentUserContext returns the stored per-user context for this
// agent relevant to question, or "" when no store is configured.
func (h *GeneralHandler) readPersistentUserContext(ctx context.Context, userID, question string) string {
	if h.userContextStore == nil || h.agentID == "" || userID == "" {
		return ""
	}
	return h.userContextStore.Context(ctx, h.agentID, userID, question)
}

// persistUserContext records a (question, answer) turn for the user. Called
// once per fully-completed request; errors are logged inside the store.
func (h *GeneralHandler) persistUserContext(ctx context.Context, userID, question, answer string) {
	if h.userContextStore == nil || h.agentID == "" || userID == "" {
		return
	}
	h.userContextStore.Append(ctx, h.agentID, userID, question, answer)
}

func (h *DebugHandler) readPersistentUserContext(ctx context.Context, userID, question string) string {
	if h.userContextStore == nil || h.agentID == "" || userID == "" {
		return ""
	}
	return h.userContextStore.Context(ctx, h.agentID, userID, question)
}

func (h *DebugHandler) persistUserContext(ctx context.Context, userID, question, answer string) {
	if h.userContextStore == nil || h.agentID == "" || userID == "" {
		return
	}
	h.userContextStore.Append(ctx, h.agentID, userID, question, answer)
}
