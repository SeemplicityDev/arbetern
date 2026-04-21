package commands

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/justmike1/arbetern/github"
	"github.com/justmike1/arbetern/llm"
	"github.com/justmike1/arbetern/slack"
)

const channelHistoryLimit = 20

type DebugHandler struct {
	slackClient      SlackClient
	ghClient         *github.Client
	modelsClient     *llm.Client
	contextProvider  *ContextProvider
	memory           *ConversationMemory
	prompts          PromptProvider
	userContext      string
	agentID          string
	userContextStore *UserContextStore
}

func (h *DebugHandler) Execute(channelID, userID, text, responseURL, auditTS string) {
	ctx := context.Background()

	channelContext, err := h.contextProvider.GetFreshChannelContext(channelID)
	if err != nil {
		log.Printf("[user=%s channel=%s] failed to fetch channel context: %v", userID, channelID, err)
		replyOrThread(h.slackClient, channelID, responseURL, auditTS, fmt.Sprintf("Failed to read channel history: %v", err))
		return
	}

	if channelContext == "(no recent messages)" || channelContext == "(no recent messages with content)" {
		replyOrThread(h.slackClient, channelID, responseURL, auditTS, "No messages found in this channel to analyze.")
		return
	}

	workflowLogs := fetchWorkflowLogsBulk(ctx, h.ghClient, channelContext+"\n"+text, userID, channelID)

	systemPrompt := h.prompts.SystemPrompt("debug")
	systemPrompt = strings.Replace(systemPrompt, "{{USER_CONTEXT}}", h.userContext, 1)
	if persistent := h.readPersistentUserContext(userID); persistent != "" {
		systemPrompt += fmt.Sprintf("\n\nRecurring topics this user has asked about previously (may hint at current intent):\n%s", persistent)
	}

	userPrompt := fmt.Sprintf("Here are the recent messages from the channel:\n\n%s\n\nUser request: %s", channelContext, text)
	if workflowLogs != "" {
		userPrompt += fmt.Sprintf("\n\nI also fetched the GitHub Actions workflow run details and logs for URLs found in the messages:\n\n%s", workflowLogs)
	}

	response, usage, err := h.modelsClient.Complete(ctx, systemPrompt, userPrompt)
	if err != nil {
		log.Printf("[user=%s channel=%s] LLM completion failed: %v", userID, channelID, err)
		_ = slack.RespondToURL(responseURL, fmt.Sprintf("Failed to analyze messages: %v", err), true)
		return
	}

	log.Printf("[user=%s channel=%s] debug analysis completed successfully", userID, channelID)
	h.memory.SetAssistantResponse(channelID, userID, response)
	h.persistUserContext(userID, text, response)
	stamp := llm.FormatUsageStamp(usage, h.modelsClient.Model())
	replyOrThread(h.slackClient, channelID, responseURL, auditTS, response+stamp)
}
