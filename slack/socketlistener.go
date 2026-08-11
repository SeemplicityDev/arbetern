package slack

import (
	"fmt"
	"log"
	"os"
	"sync/atomic"

	slacklib "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/justmike1/arbetern/internal/safego"
)

// ThreadReplyHandler is called when a user sends a message in a tracked thread.
// channelID, threadTS identify the thread; userID is the message author; text
// is the message body.
type ThreadReplyHandler func(channelID, threadTS, userID, text string)

// SlashCommandHandler is called when a slash command arrives via Socket Mode.
// command is the slash command name (e.g. "/seihin"), channelID is where it was
// invoked, userID is who invoked it, text is the argument text, and responseURL
// is the ephemeral response URL Slack provides.
type SlashCommandHandler func(command, channelID, userID, text, responseURL string)

// SocketListener connects to Slack via Socket Mode (outbound WebSocket) and
// dispatches thread reply messages to a handler. No inbound URL configuration
// is needed — the app connects to Slack, not the other way around.
type SocketListener struct {
	smClient            *socketmode.Client
	botUserID           string
	threadReplyHandler  ThreadReplyHandler
	slashCommandHandler SlashCommandHandler
	debug               bool
	connected           atomic.Bool
	eventCount          atomic.Int64
}

// NewSocketListener creates a Socket Mode listener.
// appToken is the Slack app-level token (xapp-...) with connections:write scope.
// botToken is the normal bot token (xoxb-...).
// botUserID is the bot's own Slack user ID (used to ignore self-messages).
// Set env SOCKET_MODE_DEBUG=1 to enable verbose wire-level logging.
func NewSocketListener(appToken, botToken, botUserID string, handler ThreadReplyHandler, slashHandler SlashCommandHandler) *SocketListener {
	debug := os.Getenv("SOCKET_MODE_DEBUG") == "1"

	apiOpts := []slacklib.Option{
		slacklib.OptionAppLevelToken(appToken),
	}
	if debug {
		apiOpts = append(apiOpts, slacklib.OptionDebug(true))
		apiOpts = append(apiOpts, slacklib.OptionLog(log.New(os.Stdout, "[slack-api] ", log.LstdFlags)))
	}

	api := slacklib.New(botToken, apiOpts...)

	smOpts := []socketmode.Option{}
	if debug {
		smOpts = append(smOpts, socketmode.OptionDebug(true))
		smOpts = append(smOpts, socketmode.OptionLog(log.New(os.Stdout, "[socket-wire] ", log.LstdFlags)))
	}

	smClient := socketmode.New(api, smOpts...)

	return &SocketListener{
		smClient:            smClient,
		botUserID:           botUserID,
		threadReplyHandler:  handler,
		slashCommandHandler: slashHandler,
		debug:               debug,
	}
}

// Start connects to Slack and begins listening for events in a blocking loop.
// Run this in a goroutine. It reconnects automatically on disconnection.
func (sl *SocketListener) Start() {
	safego.Go("slack: socket event loop", sl.handleEvents)

	log.Printf("[socket-mode] connecting to Slack (debug=%v)...", sl.debug)
	if err := sl.smClient.Run(); err != nil {
		log.Printf("[socket-mode] fatal: %v", err)
	}
}

// handleEvents processes incoming Socket Mode events. Each event is guarded
// individually rather than at the goroutine boundary: dispatchEvent runs
// handleEventsAPI inline, so recovering outside this loop would leave the
// process up but deaf to Slack.
func (sl *SocketListener) handleEvents() {
	for evt := range sl.smClient.Events {
		sl.eventCount.Add(1)
		safego.Run("slack: socket event "+string(evt.Type), func() { sl.dispatchEvent(evt) })
	}
	log.Printf("[socket-mode] event channel closed — listener stopped")
}

// dispatchEvent routes a single Socket Mode event.
func (sl *SocketListener) dispatchEvent(evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeConnecting:
		// Only log if we were previously connected (suppress initial spam).
		if sl.connected.Load() {
			log.Printf("[socket-mode] reconnecting...")
		}

	case socketmode.EventTypeConnected:
		wasConnected := sl.connected.Swap(true)
		if !wasConnected {
			log.Printf("[socket-mode] connected (events processed: %d)", sl.eventCount.Load())
		}

	case socketmode.EventTypeConnectionError:
		sl.connected.Store(false)
		log.Printf("[socket-mode] connection error, will retry...")

	case socketmode.EventTypeHello:
		log.Printf("[socket-mode] received hello from Slack")

	case socketmode.EventTypeEventsAPI:
		eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			log.Printf("[socket-mode] WARNING: EventsAPI event data is %T (expected slackevents.EventsAPIEvent), skipping",
				evt.Data)
			if evt.Request != nil {
				_ = sl.smClient.Ack(*evt.Request)
			}
			return
		}

		// Acknowledge the event immediately to prevent Slack retries.
		if evt.Request != nil {
			_ = sl.smClient.Ack(*evt.Request)
		}

		sl.handleEventsAPI(eventsAPIEvent)

	case socketmode.EventTypeInteractive:
		log.Printf("[socket-mode] interactive event received (ignoring)")
		if evt.Request != nil {
			_ = sl.smClient.Ack(*evt.Request)
		}

	case socketmode.EventTypeSlashCommand:
		cmd, ok := evt.Data.(slacklib.SlashCommand)
		if !ok {
			log.Printf("[socket-mode] WARNING: slash command data is %T (expected slack.SlashCommand), skipping", evt.Data)
			if evt.Request != nil {
				_ = sl.smClient.Ack(*evt.Request)
			}
			return
		}

		// Acknowledge immediately so Slack doesn't show a timeout error.
		if evt.Request != nil {
			_ = sl.smClient.Ack(*evt.Request, map[string]interface{}{
				"text": "Processing your request...",
			})
		}

		log.Printf("[socket-mode] slash command: command=%s channel=%s user=%s text=%q",
			cmd.Command, cmd.ChannelID, cmd.UserID, truncate(cmd.Text, 80))

		if sl.slashCommandHandler != nil {
			safego.Go("slack: slash command "+cmd.Command, func() {
				sl.slashCommandHandler(cmd.Command, cmd.ChannelID, cmd.UserID, cmd.Text, cmd.ResponseURL)
			})
		}

	default:
		log.Printf("[socket-mode] unhandled event type: %s (data type: %T)",
			evt.Type, evt.Data)
		// Acknowledge unknown event types to avoid retries.
		if evt.Request != nil {
			_ = sl.smClient.Ack(*evt.Request)
		}
	}
}

// handleEventsAPI processes Events API payloads delivered via Socket Mode.
func (sl *SocketListener) handleEventsAPI(event slackevents.EventsAPIEvent) {
	log.Printf("[socket-mode] events-api: type=%s inner=%s",
		event.Type, event.InnerEvent.Type)

	if event.Type != slackevents.CallbackEvent {
		log.Printf("[socket-mode] events-api: skipping non-callback event type %q", event.Type)
		return
	}

	innerData := event.InnerEvent.Data
	if innerData == nil {
		log.Printf("[socket-mode] events-api: inner event data is nil (inner type=%s)", event.InnerEvent.Type)
		return
	}

	switch ev := innerData.(type) {
	case *slackevents.MessageEvent:
		sl.handleMessage(ev)
	default:
		log.Printf("[socket-mode] events-api: unhandled inner event type %T (event type: %s)",
			innerData, event.InnerEvent.Type)
	}
}

// handleMessage processes a message event, filtering for actionable thread replies.
func (sl *SocketListener) handleMessage(ev *slackevents.MessageEvent) {
	// Log every message event for diagnostics.
	log.Printf("[socket-mode] message: channel=%s user=%s thread_ts=%q sub_type=%q bot_id=%q text=%q",
		ev.Channel, ev.User, ev.ThreadTimeStamp, ev.SubType, ev.BotID, truncate(ev.Text, 80))

	// Only handle regular user messages (no subtypes like message_changed, bot_message, etc.).
	if ev.SubType != "" {
		log.Printf("[socket-mode] message: skipping subtype=%q", ev.SubType)
		return
	}
	if ev.ThreadTimeStamp == "" {
		log.Printf("[socket-mode] message: skipping non-thread message")
		return // not a thread reply
	}
	if ev.BotID != "" {
		log.Printf("[socket-mode] message: skipping bot message (bot_id=%s)", ev.BotID)
		return
	}
	if ev.User == sl.botUserID {
		log.Printf("[socket-mode] message: skipping own message (user=%s)", ev.User)
		return
	}

	log.Printf("[socket-mode] thread reply: channel=%s thread=%s user=%s",
		ev.Channel, ev.ThreadTimeStamp, ev.User)

	safego.Go("slack: thread reply", func() {
		sl.threadReplyHandler(ev.Channel, ev.ThreadTimeStamp, ev.User, ev.Text)
	})
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("…(%d more)", len(s)-max)
}
