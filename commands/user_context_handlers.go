package commands

// readPersistentUserContext returns the on-disk per-user context for this
// agent, or empty string if no store is configured or no file exists yet.
func (h *GeneralHandler) readPersistentUserContext(userID string) string {
	if h.userContextStore == nil || h.agentID == "" || userID == "" {
		return ""
	}
	return h.userContextStore.Read(h.agentID, userID)
}

// persistUserContext appends a (question, answer) turn to the user's
// context file. Called once per fully-completed request; errors are
// swallowed by the store and logged internally.
func (h *GeneralHandler) persistUserContext(userID, question, answer string) {
	if h.userContextStore == nil || h.agentID == "" || userID == "" {
		return
	}
	h.userContextStore.Append(h.agentID, userID, question, answer)
}

// readPersistentUserContext returns the on-disk per-user context for this
// agent, or empty string if no store is configured.
func (h *DebugHandler) readPersistentUserContext(userID string) string {
	if h.userContextStore == nil || h.agentID == "" || userID == "" {
		return ""
	}
	return h.userContextStore.Read(h.agentID, userID)
}

// persistUserContext appends a (question, answer) turn to the user's
// context file.
func (h *DebugHandler) persistUserContext(userID, question, answer string) {
	if h.userContextStore == nil || h.agentID == "" || userID == "" {
		return
	}
	h.userContextStore.Append(h.agentID, userID, question, answer)
}
