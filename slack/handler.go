package slack

import (
	"io"
	"log"
	"net/http"

	slacklib "github.com/slack-go/slack"

	"github.com/justmike1/arbetern/internal/safego"
)

type CommandHandler func(channelID, userID, text, responseURL string)

type Handler struct {
	signingSecret  string
	commandHandler CommandHandler
}

func NewHandler(signingSecret string, commandHandler CommandHandler) *Handler {
	return &Handler{
		signingSecret:  signingSecret,
		commandHandler: commandHandler,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	verifier, err := slacklib.NewSecretsVerifier(r.Header, h.signingSecret)
	if err != nil {
		log.Printf("failed to create secrets verifier: %v", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	r.Body = io.NopCloser(io.TeeReader(r.Body, &verifier))

	cmd, err := slacklib.SlashCommandParse(r)
	if err != nil {
		log.Printf("failed to parse slash command: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if err := verifier.Ensure(); err != nil {
		log.Printf("signature verification failed: %v", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("Processing your request..."))

	// Outside net/http's handler recovery: this goroutine carries the whole
	// command path, so an unguarded panic here would take the process down.
	safego.Go("slack: slash command "+cmd.Command, func() {
		h.commandHandler(cmd.ChannelID, cmd.UserID, cmd.Text, cmd.ResponseURL)
	})
}
