package commands

import (
	"fmt"
	"sync"

	slacklib "github.com/slack-go/slack"
)

// DirectClient implements SlackClient for non-Slack callers (e.g. the HTTP
// query API). Instead of posting to Slack, it captures all responses into an
// internal buffer that can be retrieved after the handler completes.
type DirectClient struct {
	mu        sync.Mutex
	responses []string
	context   string // optional caller-supplied context injected as channel history
}

// NewDirectClient returns a DirectClient. context is an optional string that
// will be returned as a synthetic channel message when the handler fetches
// channel history, allowing the caller to supply background context.
func NewDirectClient(context string) *DirectClient {
	return &DirectClient{context: context}
}

func (d *DirectClient) FetchChannelHistory(_ string, _ int) ([]slacklib.Message, error) {
	if d.context == "" {
		return nil, nil
	}
	return []slacklib.Message{
		{Msg: slacklib.Msg{Text: d.context, Username: "context"}},
	}, nil
}

func (d *DirectClient) FetchThreadReplies(_, _ string, _ int) ([]slacklib.Message, error) {
	return nil, nil
}

func (d *DirectClient) PostMessage(_ string, text string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.responses = append(d.responses, text)
	return fmt.Sprintf("direct-%d", len(d.responses)), nil
}

func (d *DirectClient) PostThreadReply(_, _ string, text string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.responses = append(d.responses, text)
	return nil
}

func (d *DirectClient) GetPermalink(_, _ string) (string, error) {
	return "", nil
}

func (d *DirectClient) GetUserInfo(userID string) (*slacklib.User, error) {
	return &slacklib.User{
		ID:       userID,
		RealName: "API User",
		Profile: slacklib.UserProfile{
			DisplayName: "api-user",
			Email:       "api@direct",
		},
	}, nil
}

func (d *DirectClient) UploadFileSnippet(_, _, _, _, _, _ string) (string, error) {
	return "", nil
}

// Responses returns all captured response texts in order.
func (d *DirectClient) Responses() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.responses))
	copy(out, d.responses)
	return out
}
