package slack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/slack-go/slack"
)

type Client struct {
	api   *slack.Client
	token string
}

func NewClient(botToken string) *Client {
	return &Client{api: slack.New(botToken), token: botToken}
}

func (c *Client) FetchChannelHistory(channelID string, limit int) ([]slack.Message, error) {
	params := &slack.GetConversationHistoryParameters{
		ChannelID: channelID,
		Limit:     limit,
	}

	resp, err := c.api.GetConversationHistory(params)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch channel history: %w", err)
	}

	return resp.Messages, nil
}

func (c *Client) PostMessage(channelID, text string) (string, error) {
	_, ts, err := c.api.PostMessage(channelID, slack.MsgOptionText(text, false))
	if err != nil {
		return "", fmt.Errorf("failed to post message: %w", err)
	}
	return ts, nil
}

func (c *Client) PostThreadReply(channelID, threadTS, text string) error {
	_, _, err := c.api.PostMessage(channelID, slack.MsgOptionText(text, false), slack.MsgOptionTS(threadTS))
	if err != nil {
		return fmt.Errorf("failed to post thread reply: %w", err)
	}
	return nil
}

func (c *Client) FetchThreadReplies(channelID, threadTS string, limit int) ([]slack.Message, error) {
	msgs, _, _, err := c.api.GetConversationReplies(&slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: threadTS,
		Limit:     limit,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch thread replies: %w", err)
	}
	return msgs, nil
}

func (c *Client) PostEphemeral(channelID, userID, text string) error {
	_, err := c.api.PostEphemeral(channelID, userID, slack.MsgOptionText(text, false))
	if err != nil {
		return fmt.Errorf("failed to post ephemeral message: %w", err)
	}
	return nil
}

// GetPermalink returns the permanent URL for a specific message in a channel.
func (c *Client) GetPermalink(channelID, messageTS string) (string, error) {
	permalink, err := c.api.GetPermalink(&slack.PermalinkParameters{
		Channel: channelID,
		Ts:      messageTS,
	})
	if err != nil {
		return "", fmt.Errorf("failed to get permalink: %w", err)
	}
	return permalink, nil
}

// GetUserInfo returns profile information for a Slack user by their user ID.
func (c *Client) GetUserInfo(userID string) (*slack.User, error) {
	user, err := c.api.GetUserInfo(userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get user info: %w", err)
	}
	return user, nil
}

// GetTeamURL returns the Slack workspace URL (e.g. "https://myorg.slack.com/").
func (c *Client) GetTeamURL() (string, error) {
	resp, err := c.api.AuthTest()
	if err != nil {
		return "", fmt.Errorf("failed to call auth.test: %w", err)
	}
	return resp.URL, nil
}

// GetBotUserID returns the Slack user ID of the bot token.
func (c *Client) GetBotUserID() (string, error) {
	resp, err := c.api.AuthTest()
	if err != nil {
		return "", fmt.Errorf("failed to call auth.test: %w", err)
	}
	return resp.UserID, nil
}

// GetUserGroupMembers returns the list of user IDs in a Slack user group (subteam).
// Requires the usergroups:read scope on the bot token.
func (c *Client) GetUserGroupMembers(groupID string) ([]string, error) {
	members, err := c.api.GetUserGroupMembers(groupID)
	if err != nil {
		return nil, fmt.Errorf("failed to get members for user group %s: %w", groupID, err)
	}
	return members, nil
}

// GetBotScopes calls auth.test via a raw HTTP request and reads the
// x-oauth-scopes response header to determine the bot token's granted scopes.
func (c *Client) GetBotScopes() ([]string, error) {
	data := url.Values{"token": {c.token}}
	resp, err := http.PostForm("https://slack.com/api/auth.test", data)
	if err != nil {
		return nil, fmt.Errorf("slack auth.test request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw := resp.Header.Get("X-OAuth-Scopes")
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	scopes := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			scopes = append(scopes, s)
		}
	}
	return scopes, nil
}

type webhookPayload struct {
	ResponseType string `json:"response_type"`
	Text         string `json:"text"`
}

func RespondToURL(responseURL, text string, ephemeral bool) error {
	respType := "in_channel"
	if ephemeral {
		respType = "ephemeral"
	}

	payload, err := json.Marshal(webhookPayload{
		ResponseType: respType,
		Text:         text,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal response payload: %w", err)
	}

	resp, err := http.Post(responseURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to post to response_url: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("response_url returned status %d", resp.StatusCode)
	}

	return nil
}
