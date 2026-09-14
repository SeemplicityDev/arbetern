package chat

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/justmike1/arbetern/internal/store"
)

// maxMessageBytes bounds an inbound chat message to keep the LLM prompt and
// on-disk transcript a sane size.
const maxMessageBytes = 8 << 10 // 8 KiB

// postRequest is the JSON body accepted when sending a message.
type postRequest struct {
	Message string `json:"message"`
	User    string `json:"user,omitempty"`
}

// renameRequest is the JSON body accepted by PATCH on a conversation.
type renameRequest struct {
	Title string `json:"title"`
}

// RegisterRoutes mounts the centralized, multi-conversation chat API onto
// apiMux (ChatGPT/Claude-style threads):
//
//	GET    /api/chat/<agent>/conversations        → list conversation summaries
//	POST   /api/chat/<agent>/conversations        → create a new conversation
//	GET    /api/chat/<agent>/conversations/<id>   → full conversation (messages)
//	POST   /api/chat/<agent>/conversations/<id>   → send a message; the reply is produced in the background and GET reports `pending` until it lands
//	PATCH  /api/chat/<agent>/conversations/<id>   → rename the conversation
//	DELETE /api/chat/<agent>/conversations/<id>   → delete the conversation
//
// Requests for unknown agents return 404; requests for agents that have not
// enabled chat return 403. Access control is enforced upstream by the global
// IP gate in main.
func (r *Registry) RegisterRoutes(apiMux *http.ServeMux, knownAgents map[string]bool) {
	apiMux.HandleFunc("/api/chat/", func(w http.ResponseWriter, req *http.Request) {
		rest := strings.Trim(strings.TrimPrefix(req.URL.Path, "/api/chat/"), "/")
		parts := strings.Split(rest, "/")
		agent := parts[0]
		if agent == "" || !store.AgentRe.MatchString(agent) || !knownAgents[agent] {
			http.Error(w, "unknown agent", http.StatusNotFound)
			return
		}
		if !r.IsEnabled(agent) {
			http.Error(w, "chat is not enabled for this agent", http.StatusForbidden)
			return
		}
		if !r.authorized(req, agent) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// Expect /api/chat/<agent>/conversations[/<id>].
		if len(parts) < 2 || parts[1] != "conversations" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if len(parts) == 2 {
			r.handleCollection(w, req, agent)
			return
		}
		id := parts[2]
		if !store.IDRe.MatchString(id) {
			http.Error(w, "invalid conversation id", http.StatusNotFound)
			return
		}
		r.handleConversation(w, req, agent, id)
	})
}

// handleCollection serves GET (list) and POST (create) on an agent's
// conversation collection.
func (r *Registry) handleCollection(w http.ResponseWriter, req *http.Request, agent string) {
	switch req.Method {
	case http.MethodGet:
		list, err := r.ListConversations(agent)
		if err != nil {
			http.Error(w, "failed to list conversations", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, list)

	case http.MethodPost:
		conv, err := r.CreateConversation(req.Context(), agent)
		if err != nil {
			http.Error(w, "failed to create conversation", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, conv)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleConversation serves GET/POST/PATCH/DELETE on a single conversation.
func (r *Registry) handleConversation(w http.ResponseWriter, req *http.Request, agent, id string) {
	switch req.Method {
	case http.MethodGet:
		conv, err := r.Conversation(agent, id)
		if err != nil {
			http.Error(w, "failed to load conversation", http.StatusInternalServerError)
			return
		}
		if conv == nil {
			http.Error(w, "conversation not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, conv)

	case http.MethodPost:
		var body postRequest
		dec := json.NewDecoder(io.LimitReader(req.Body, maxMessageBytes+1024))
		if err := dec.Decode(&body); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		msg := strings.TrimSpace(body.Message)
		if msg == "" {
			http.Error(w, "message must not be empty", http.StatusBadRequest)
			return
		}
		if len(msg) > maxMessageBytes {
			msg = msg[:maxMessageBytes]
		}
		// Prefer the OAuth-proxy-verified email (when a proxy is in front) over
		// any client-supplied name, so an authenticated sender is attributed by
		// their real identity instead of "Anonymous". Falls back to the free-text
		// body name for local/unauthenticated use.
		user := strings.TrimSpace(body.User)
		if email := r.userFor(req); email != "" {
			user = email
		}
		if len(user) > 80 {
			user = user[:80]
		}

		if err := r.Start(agent, id, user, msg); err != nil {
			switch {
			case errors.Is(err, ErrNotFound):
				http.Error(w, "conversation not found", http.StatusNotFound)
			case errors.Is(err, ErrBusy):
				http.Error(w, err.Error(), http.StatusConflict)
			default:
				http.Error(w, "failed to start reply: "+err.Error(), http.StatusBadGateway)
			}
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})

	case http.MethodPatch:
		var body renameRequest
		dec := json.NewDecoder(io.LimitReader(req.Body, 4<<10))
		if err := dec.Decode(&body); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		title := strings.TrimSpace(body.Title)
		if title == "" {
			http.Error(w, "title must not be empty", http.StatusBadRequest)
			return
		}
		if len(title) > 120 {
			title = title[:120]
		}
		conv, err := r.RenameConversation(req.Context(), agent, id, title)
		if err != nil {
			http.Error(w, "failed to rename conversation", http.StatusInternalServerError)
			return
		}
		if conv == nil {
			http.Error(w, "conversation not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, conv)

	case http.MethodDelete:
		if err := r.DeleteConversation(req.Context(), agent, id); err != nil {
			http.Error(w, "failed to delete conversation", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
