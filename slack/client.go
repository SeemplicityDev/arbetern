package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/slack-go/slack"
)

// Response body size limit for reading error bodies.
const maxErrorResponseBody = 1 << 20 // 1 MB

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

// FetchChannelHistoryWindow returns up to `limit` messages posted after the
// `oldest` Slack timestamp (exclusive). Pass an empty string or "0" for
// `oldest` to return the most recent `limit` messages regardless of age.
// Slack returns messages newest-first.
func (c *Client) FetchChannelHistoryWindow(channelID, oldest string, limit int) ([]slack.Message, error) {
	params := &slack.GetConversationHistoryParameters{
		ChannelID: channelID,
		Limit:     limit,
	}
	if oldest != "" {
		params.Oldest = oldest
	}

	resp, err := c.api.GetConversationHistory(params)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch channel history window: %w", err)
	}

	return resp.Messages, nil
}

func (c *Client) PostMessage(channelID, text string) (string, error) {
	_, ts, err := c.api.PostMessage(channelID,
		slack.MsgOptionText(text, false),
		slack.MsgOptionDisableLinkUnfurl(),
		slack.MsgOptionDisableMediaUnfurl(),
	)
	if err != nil {
		return "", fmt.Errorf("failed to post message: %w", err)
	}
	return ts, nil
}

// PostMessageInThread posts to channelID as a threaded reply under threadTS
// and returns the new message's ts. When threadTS is empty, behaves like
// PostMessage (posts as a top-level message).
func (c *Client) PostMessageInThread(channelID, threadTS, text string) (string, error) {
	opts := []slack.MsgOption{
		slack.MsgOptionText(text, false),
		slack.MsgOptionDisableLinkUnfurl(),
		slack.MsgOptionDisableMediaUnfurl(),
	}
	if strings.TrimSpace(threadTS) != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, ts, err := c.api.PostMessage(channelID, opts...)
	if err != nil {
		return "", fmt.Errorf("failed to post message: %w", err)
	}
	return ts, nil
}

func (c *Client) PostThreadReply(channelID, threadTS, text string) error {
	_, _, err := c.api.PostMessage(channelID,
		slack.MsgOptionText(text, false),
		slack.MsgOptionTS(threadTS),
		slack.MsgOptionDisableLinkUnfurl(),
		slack.MsgOptionDisableMediaUnfurl(),
	)
	if err != nil {
		return fmt.Errorf("failed to post thread reply: %w", err)
	}
	return nil
}

// PostBlocks posts a Block Kit message with a fallback text string (shown by
// clients that can't render blocks, e.g. mobile notifications). Returns the
// message ts on success. Blocks must be built by the caller using
// github.com/slack-go/slack.Block implementations.
func (c *Client) PostBlocks(channelID, fallback string, blocks []slack.Block) (string, error) {
	_, ts, err := c.api.PostMessage(channelID,
		slack.MsgOptionText(fallback, false),
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionDisableLinkUnfurl(),
		slack.MsgOptionDisableMediaUnfurl(),
	)
	if err != nil {
		return "", fmt.Errorf("failed to post blocks: %w", err)
	}
	return ts, nil
}

// PostThreadBlocks posts a Block Kit message as a reply in an existing thread.
func (c *Client) PostThreadBlocks(channelID, threadTS, fallback string, blocks []slack.Block) error {
	_, _, err := c.api.PostMessage(channelID,
		slack.MsgOptionText(fallback, false),
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionTS(threadTS),
		slack.MsgOptionDisableLinkUnfurl(),
		slack.MsgOptionDisableMediaUnfurl(),
	)
	if err != nil {
		return fmt.Errorf("failed to post thread blocks: %w", err)
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
	_, err := c.api.PostEphemeral(channelID, userID,
		slack.MsgOptionText(text, false),
		slack.MsgOptionDisableLinkUnfurl(),
		slack.MsgOptionDisableMediaUnfurl(),
	)
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

// GetUserByEmail looks up a Slack user by their email address.
// Requires the users:read.email scope on the bot token.
func (c *Client) GetUserByEmail(email string) (*slack.User, error) {
	user, err := c.api.GetUserByEmail(email)
	if err != nil {
		// Do not embed the raw email in the wrapped error: it is sensitive
		// (CWE-312) and propagates into caller logs. The underlying Slack API
		// reason (e.g. "users_not_found") is preserved via %w, and callers
		// already hold the email for redacted logging.
		return nil, fmt.Errorf("failed to look up user by email: %w", err)
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
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

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

// UploadFileSnippet uploads a text snippet as a Slack file using the
// files.getUploadURLExternal / files.completeUploadExternal flow.
// It posts the snippet into the given channel (and optionally a thread).
func (c *Client) UploadFileSnippet(channelID, threadTS, filename, title, content, filetype string) (string, error) {
	ctx := context.Background()
	urlResp, err := c.api.GetUploadURLExternalContext(ctx, slack.GetUploadURLExternalParameters{
		FileName:    filename,
		FileSize:    len(content),
		SnippetType: filetype,
	})
	if err != nil {
		return "", fmt.Errorf("failed to get upload URL: %w", err)
	}

	// Upload the content with an explicit Content-Length. slack-go's
	// UploadToURL streams the body through an io.Pipe, which sends the request
	// with chunked transfer-encoding and no Content-Length; Slack's external
	// upload endpoint frequently stores a 0-byte file in that case ("empty
	// file" errors). Building the multipart body in a buffer lets net/http set
	// Content-Length, which makes the upload reliable.
	if err := c.uploadContentToURL(ctx, urlResp.UploadURL, filename, content); err != nil {
		return "", fmt.Errorf("failed to upload file content: %w", err)
	}

	completeParams := slack.CompleteUploadExternalParameters{
		Files:   []slack.FileSummary{{ID: urlResp.FileID, Title: title}},
		Channel: channelID,
	}
	if threadTS != "" {
		completeParams.ThreadTimestamp = threadTS
	}
	resp, err := c.api.CompleteUploadExternalContext(ctx, completeParams)
	if err != nil {
		return "", fmt.Errorf("failed to complete file upload: %w", err)
	}
	if len(resp.Files) > 0 {
		return resp.Files[0].ID, nil
	}
	return urlResp.FileID, nil
}

// uploadContentToURL POSTs content to a Slack external upload URL as a
// multipart/form-data body built in memory, so net/http can set an accurate
// Content-Length header. This avoids the chunked-transfer-encoding upload that
// slack-go's UploadToURL performs, which Slack's upload endpoint can store as a
// 0-byte file.
func (c *Client) uploadContentToURL(ctx context.Context, uploadURL, filename, content string) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return fmt.Errorf("failed to create multipart field: %w", err)
	}
	if _, err := io.Copy(fw, strings.NewReader(content)); err != nil {
		return fmt.Errorf("failed to write upload body: %w", err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("failed to finalize upload body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, &buf)
	if err != nil {
		return fmt.Errorf("failed to build upload request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to upload file content: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorResponseBody))
		return fmt.Errorf("upload returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
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
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorResponseBody))
		return fmt.Errorf("response_url returned status %d: %s", resp.StatusCode, string(errBody))
	}

	return nil
}
