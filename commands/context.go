package commands

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	slacklib "github.com/slack-go/slack"
)

const (
	contextMessageLimit = 30
	contextCacheTTL     = 30 * time.Second
)

type ContextProvider struct {
	slackClient SlackClient
	mu          sync.Mutex
	cache       map[string]*contextEntry
}

type contextEntry struct {
	messages  []slacklib.Message
	fetchedAt time.Time
}

func NewContextProvider(slackClient SlackClient) *ContextProvider {
	return &ContextProvider{
		slackClient: slackClient,
		cache:       make(map[string]*contextEntry),
	}
}

func (cp *ContextProvider) GetChannelContext(channelID string) (string, error) {
	cp.mu.Lock()
	entry, ok := cp.cache[channelID]
	if ok && time.Since(entry.fetchedAt) < contextCacheTTL {
		cp.mu.Unlock()
		return formatMessages(entry.messages), nil
	}
	cp.mu.Unlock()

	return cp.GetFreshChannelContext(channelID)
}

func (cp *ContextProvider) GetFreshChannelContext(channelID string) (string, error) {
	messages, err := cp.slackClient.FetchChannelHistory(channelID, contextMessageLimit)
	if err != nil {
		return "", fmt.Errorf("failed to fetch channel context: %w", err)
	}

	cp.mu.Lock()
	cp.cache[channelID] = &contextEntry{
		messages:  messages,
		fetchedAt: time.Now(),
	}
	cp.mu.Unlock()

	return formatMessages(messages), nil
}

func formatMessages(messages []slacklib.Message) string {
	if len(messages) == 0 {
		return "(no recent messages)"
	}

	total := len(messages)
	var sb strings.Builder
	fmt.Fprintf(&sb, "Messages listed from NEWEST (message 1) to OLDEST (message %d):\n\n", total)
	idx := 1
	for i := 0; i < total; i++ {
		msg := messages[i]
		text := extractMessageContent(msg)
		if text == "" {
			continue
		}
		ts := msg.Timestamp
		if t, err := tsToTime(ts); err == nil {
			ts = t.Format("15:04:05")
		}
		sender := msg.User
		if sender == "" && msg.Username != "" {
			sender = msg.Username
		}
		isBot := msg.BotID != ""
		if sender == "" && isBot {
			sender = "bot:" + msg.BotID
		}
		label := ""
		if idx == 1 {
			label = " [LATEST]"
		}
		if isBot {
			label += " [BOT]"
		}
		fmt.Fprintf(&sb, "Message %d%s [%s @%s] (thread_ts=%s): %s\n", idx, label, ts, sender, msg.Timestamp, text)
		idx++
	}
	if idx == 1 {
		return "(no recent messages with content)"
	}
	return sb.String()
}

func extractMessageContent(msg slacklib.Message) string {
	var parts []string

	if msg.Text != "" {
		parts = append(parts, expandSlackLinks(msg.Text))
	}

	for _, att := range msg.Attachments {
		var attParts []string
		if att.Pretext != "" {
			attParts = append(attParts, expandSlackLinks(att.Pretext))
		}
		if att.Title != "" {
			title := att.Title
			if att.TitleLink != "" {
				title += " (" + att.TitleLink + ")"
			}
			attParts = append(attParts, title)
		}
		if att.Text != "" {
			attParts = append(attParts, expandSlackLinks(att.Text))
		}
		for _, f := range att.Fields {
			attParts = append(attParts, f.Title+": "+f.Value)
		}
		for _, action := range att.Actions {
			if action.URL != "" {
				attParts = append(attParts, action.Text+": "+action.URL)
			}
		}
		attParts = append(attParts, extractBlockURLs(att.Blocks.BlockSet)...)
		if len(attParts) == 0 && att.Fallback != "" {
			attParts = append(attParts, expandSlackLinks(att.Fallback))
		}
		if len(attParts) > 0 {
			parts = append(parts, strings.Join(attParts, "\n"))
		}
	}

	parts = append(parts, extractBlockURLs(msg.Blocks.BlockSet)...)

	return strings.Join(parts, "\n---\n")
}

func extractBlockURLs(blocks []slacklib.Block) []string {
	var urls []string
	for _, block := range blocks {
		switch b := block.(type) {
		case *slacklib.ActionBlock:
			if b.Elements != nil {
				for _, elem := range b.Elements.ElementSet {
					if btn, ok := elem.(*slacklib.ButtonBlockElement); ok && btn.URL != "" {
						label := btn.ActionID
						if btn.Text != nil {
							label = btn.Text.Text
						}
						urls = append(urls, label+": "+btn.URL)
					}
				}
			}
		case *slacklib.SectionBlock:
			if b.Accessory != nil && b.Accessory.ButtonElement != nil && b.Accessory.ButtonElement.URL != "" {
				btn := b.Accessory.ButtonElement
				label := btn.ActionID
				if btn.Text != nil {
					label = btn.Text.Text
				}
				urls = append(urls, label+": "+btn.URL)
			}
		}
	}
	return urls
}

func tsToTime(ts string) (time.Time, error) {
	parts := strings.SplitN(ts, ".", 2)
	if len(parts) == 0 || parts[0] == "" {
		return time.Time{}, fmt.Errorf("invalid timestamp")
	}
	sec, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid timestamp: %w", err)
	}
	return time.Unix(sec, 0), nil
}

var slackLinkRe = regexp.MustCompile(`<(https?://[^|>]+)(?:\|[^>]*)?>`)

// slackThreadURLRe matches Slack thread/message URLs like:
// https://org.slack.com/archives/C01BS13KFL7/p1771847194296799
// https://org.slack.com/archives/C01BS13KFL7/p1771849373029919?thread_ts=1771847194.296799&cid=C01BS13KFL7
var slackThreadURLRe = regexp.MustCompile(`https://[^/]+\.slack\.com/archives/([A-Z0-9]+)/p(\d{10})(\d{6})`)

// ParseSlackThreadURL extracts channelID and thread_ts from a Slack message URL.
// The "p" parameter encodes the timestamp as digits without a dot (e.g., p1771847194296799 → 1771847194.296799).
// If the URL has ?thread_ts=..., that value is used; otherwise the timestamp is derived from the "p" segment.
func ParseSlackThreadURL(rawURL string) (channelID, threadTS string, err error) {
	m := slackThreadURLRe.FindStringSubmatch(rawURL)
	if m == nil {
		return "", "", fmt.Errorf("not a valid Slack message URL")
	}
	channelID = m[1]
	// Check for explicit thread_ts query param.
	if idx := strings.Index(rawURL, "thread_ts="); idx >= 0 {
		rest := rawURL[idx+len("thread_ts="):]
		if ampIdx := strings.Index(rest, "&"); ampIdx >= 0 {
			rest = rest[:ampIdx]
		}
		threadTS = rest
	} else {
		// Derive from the p-segment: p<10-digit-seconds><6-digit-microseconds> → seconds.microseconds
		threadTS = m[2] + "." + m[3]
	}
	return channelID, threadTS, nil
}

// expandSlackLinks replaces Slack mrkdwn links like <https://url|label> with "label: https://url"
// and bare <https://url> with just the URL, so workflow-run URLs become visible for extraction.
func expandSlackLinks(text string) string {
	return slackLinkRe.ReplaceAllStringFunc(text, func(match string) string {
		inner := match[1 : len(match)-1] // strip < >
		if idx := strings.Index(inner, "|"); idx >= 0 {
			url := inner[:idx]
			label := inner[idx+1:]
			return label + ": " + url
		}
		return inner
	})
}
