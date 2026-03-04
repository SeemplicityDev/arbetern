package llm

import "encoding/json"

// extractAPIErrorMessage attempts to parse an OpenAI-style JSON error body
// and return just the human-readable message. Falls back to the raw body if
// parsing fails.
func extractAPIErrorMessage(body []byte) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	// Truncate long raw bodies to keep log lines reasonable.
	if len(body) > 300 {
		return string(body[:300]) + "…"
	}
	return string(body)
}
