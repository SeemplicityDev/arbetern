package commands

import slacklib "github.com/slack-go/slack"

type SlackClient interface {
	FetchChannelHistory(channelID string, limit int) ([]slacklib.Message, error)
	FetchThreadReplies(channelID, threadTS string, limit int) ([]slacklib.Message, error)
	PostMessage(channelID, text string) (string, error)
	PostThreadReply(channelID, threadTS, text string) error
	GetPermalink(channelID, messageTS string) (string, error)
	GetUserInfo(userID string) (*slacklib.User, error)
}

// PromptProvider abstracts access to per-agent prompts.
type PromptProvider interface {
	Get(key string) string
	MustGet(key string) string
	// SystemPrompt builds the full system prompt by joining all global keys
	// (in YAML order) with the handler-specific key.
	SystemPrompt(specificKey string) string
}

// OAuthClient is implemented by any integration client that uses OAuth and
// may need a background retry before it becomes usable.
type OAuthClient interface {
	// Ready reports whether the client has successfully completed its OAuth
	// handshake and is ready to serve requests.
	Ready() bool
}
