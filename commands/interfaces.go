package commands

import (
	"github.com/justmike1/arbetern/billing"
	slacklib "github.com/slack-go/slack"
)

// UsageRecorder receives the cumulative LLM token cost of a finished turn so it
// can be aggregated for the Usage & Billing tab. Implemented by *billing.Store.
type UsageRecorder interface {
	Record(billing.Event)
}

type SlackClient interface {
	FetchChannelHistory(channelID string, limit int) ([]slacklib.Message, error)
	FetchChannelHistoryWindow(channelID, oldest string, limit int) ([]slacklib.Message, error)
	FetchThreadReplies(channelID, threadTS string, limit int) ([]slacklib.Message, error)
	PostMessage(channelID, text string) (string, error)
	PostMessageInThread(channelID, threadTS, text string) (string, error)
	PostThreadReply(channelID, threadTS, text string) error
	PostBlocks(channelID, fallback string, blocks []slacklib.Block) (string, error)
	PostThreadBlocks(channelID, threadTS, fallback string, blocks []slacklib.Block) error
	GetPermalink(channelID, messageTS string) (string, error)
	GetUserInfo(userID string) (*slacklib.User, error)
	GetUserByEmail(email string) (*slacklib.User, error)
	GetUserByName(name string) (*slacklib.User, bool, error)
	UploadFileSnippet(channelID, threadTS, filename, title, content, filetype string) (string, error)
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
