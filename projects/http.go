package projects

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/justmike1/arbetern/internal/crud"
	"github.com/justmike1/arbetern/internal/httpx"
	"github.com/justmike1/arbetern/internal/store"
)

const apiTimeout = 30 * time.Second

type consoleAPI struct {
	r         *Registry
	authorize func(*http.Request, string) bool
	userFor   func(*http.Request) string
}

// RegisterRoutes mounts the projects console API and the per-agent data routes.
func (r *Registry) RegisterRoutes(mux, apiMux *http.ServeMux, knownAgents map[string]bool, authorize func(*http.Request, string) bool, userFor func(*http.Request) string) {
	api := &consoleAPI{r: r, authorize: authorize, userFor: userFor}
	crud.Mount(mux, apiMux, knownAgents, crud.Spec{
		Kind:       "project",
		KindPlural: "projects",
		Get: func(agent, id string) (any, string, bool) {
			v, ok := r.view(agent, id)
			if !ok {
				return nil, "", false
			}
			return v, "", true
		},
		List:      func(agent string) any { return r.list(agent) },
		Delete:    r.remove,
		Custom:    api.custom,
		Authorize: authorize,
	})
	apiMux.HandleFunc("POST /api/projects", api.create)
}

// RegisterDisabledRoutes answers the projects API with 503 on deployments without the feature.
func RegisterDisabledRoutes(apiMux *http.ServeMux) {
	off := func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusServiceUnavailable, "projects are not enabled")
	}
	apiMux.HandleFunc("/api/projects", off)
	apiMux.HandleFunc("/api/projects/", off)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	httpx.WriteJSON(w, status, map[string]string{"error": msg})
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func decodeBody(w http.ResponseWriter, req *http.Request, v any) (int, error) {
	req.Body = http.MaxBytesReader(w, req.Body, maxBodyBytes)
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil {
		if extra := dec.Decode(&json.RawMessage{}); extra != io.EOF {
			err = extra
			if err == nil {
				err = errors.New("the body must hold a single JSON value")
			}
		}
	}
	var tooBig *http.MaxBytesError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &tooBig):
		return http.StatusRequestEntityTooLarge, fmt.Errorf("the request body is larger than %d bytes", maxBodyBytes)
	default:
		return http.StatusBadRequest, fmt.Errorf("invalid JSON body: %v", err)
	}
}

func decodeStrict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (a *consoleAPI) user(req *http.Request) string {
	if a.userFor == nil {
		return ""
	}
	return a.userFor(req)
}

func (a *consoleAPI) exists(ctx context.Context, w http.ResponseWriter, agent, id, action string) bool {
	_, err := a.r.project(ctx, agent, id)
	if errors.Is(err, store.ErrNotFound) {
		err = errNotFound
	}
	if err != nil {
		a.fail(w, action, err)
		return false
	}
	return true
}

func (a *consoleAPI) fail(w http.ResponseWriter, action string, err error) {
	switch {
	case isInputError(err):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, "project not found")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "the project changed while it was being saved; try again")
	default:
		log.Printf("[projects] %s: %v", action, err)
		writeError(w, http.StatusInternalServerError, "could not "+action)
	}
}

type createRequest struct {
	Agent        string   `json:"agent"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Goal         string   `json:"goal"`
	Instructions string   `json:"instructions"`
	Repo         Repo     `json:"repo"`
	Signal       Signal   `json:"signal"`
	Dispatch     Dispatch `json:"dispatch"`
	Limits       Limits   `json:"limits"`
	Interval     string   `json:"interval"`
	Enabled      *bool    `json:"enabled"`
}

func (a *consoleAPI) create(w http.ResponseWriter, req *http.Request) {
	if err := httpx.CheckSameOrigin(req); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	var body createRequest
	if status, err := decodeBody(w, req, &body); err != nil {
		writeError(w, status, err.Error())
		return
	}
	agent := strings.TrimSpace(body.Agent)
	if !a.r.knownAgent(agent) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown agent %q", agent))
		return
	}
	if a.authorize != nil && !a.authorize(req, agent) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	draft := Project{
		Agent:        agent,
		Name:         body.Name,
		Description:  body.Description,
		Goal:         body.Goal,
		Instructions: body.Instructions,
		Repo:         body.Repo,
		Signal:       body.Signal,
		Dispatch:     body.Dispatch,
		Limits:       body.Limits,
		Interval:     body.Interval,
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), apiTimeout)
	defer cancel()
	p, err := a.r.create(ctx, draft, body.Enabled == nil || *body.Enabled, a.user(req))
	if err != nil {
		a.fail(w, "create the project", err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, output(p))
}

func (a *consoleAPI) custom(w http.ResponseWriter, req *http.Request, agent, id string, sub []string) bool {
	switch {
	case len(sub) == 0 && req.Method == http.MethodPatch:
		a.update(w, req, agent, id)
	case len(sub) == 1 && sub[0] == "run":
		if req.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return true
		}
		a.run(w, req, agent, id)
	case len(sub) == 1 && sub[0] == "memory":
		if req.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return true
		}
		a.addNote(w, req, agent, id)
	case len(sub) == 2 && sub[0] == "memory" && sub[1] != "":
		if req.Method != http.MethodDelete {
			methodNotAllowed(w, http.MethodDelete)
			return true
		}
		a.deleteNote(w, agent, id, sub[1])
	default:
		return false
	}
	return true
}

func (a *consoleAPI) update(w http.ResponseWriter, req *http.Request, agent, id string) {
	var raw map[string]json.RawMessage
	if status, err := decodeBody(w, req, &raw); err != nil {
		writeError(w, status, err.Error())
		return
	}
	if len(raw) == 0 {
		writeError(w, http.StatusBadRequest, "the request changes nothing")
		return
	}
	var pt projectPatch
	text := func(v json.RawMessage) (*string, error) {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, errors.New("must be a string")
		}
		return &s, nil
	}
	for k, v := range raw {
		if string(bytes.TrimSpace(v)) == "null" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%s: must not be null", k))
			return
		}
		var err error
		switch k {
		case "name":
			pt.Name, err = text(v)
		case "description":
			pt.Description, err = text(v)
		case "goal":
			pt.Goal, err = text(v)
		case "instructions":
			pt.Instructions, err = text(v)
		case "interval":
			pt.Interval, err = text(v)
		case "repo":
			pt.Repo = new(Repo)
			err = decodeStrict(v, pt.Repo)
		case "signal":
			pt.Signal = new(Signal)
			err = decodeStrict(v, pt.Signal)
		case "dispatch":
			pt.Dispatch = new(Dispatch)
			err = decodeStrict(v, pt.Dispatch)
		case "limits":
			pt.Limits = new(Limits)
			err = decodeStrict(v, pt.Limits)
		case "enabled":
			pt.Enabled = new(bool)
			if json.Unmarshal(v, pt.Enabled) != nil {
				err = errors.New("must be a boolean")
			}
		default:
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown field %q", k))
			return
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%s: %v", k, err))
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), apiTimeout)
	defer cancel()
	p, err := a.r.update(ctx, agent, id, pt)
	if err != nil {
		a.fail(w, "update the project", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, output(p))
}

func (a *consoleAPI) run(w http.ResponseWriter, req *http.Request, agent, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), viewTimeout)
	defer cancel()
	p, err := a.r.fetchProject(ctx, agent, id)
	if errors.Is(err, store.ErrNotFound) {
		err = errNotFound
	}
	if err != nil {
		a.fail(w, "start a run", err)
		return
	}
	if !p.Enabled {
		writeError(w, http.StatusConflict, "the project is paused; resume it before running it")
		return
	}
	if err := a.r.trigger(ctx, agent, id); err != nil {
		a.fail(w, "start a run", err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func (a *consoleAPI) addNote(w http.ResponseWriter, req *http.Request, agent, id string) {
	var body struct {
		Text string `json:"text"`
	}
	if status, err := decodeBody(w, req, &body); err != nil {
		writeError(w, status, err.Error())
		return
	}
	note := strings.TrimSpace(stripControl(body.Text))
	if err := checkText("text", note, 1, maxNoteChars); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), viewTimeout)
	defer cancel()
	if !a.exists(ctx, w, agent, id, "add the note") {
		return
	}
	noteID, err := store.NewID()
	if err != nil {
		a.fail(w, "add the note", err)
		return
	}
	n := Note{ID: noteID, Text: note, Source: noteAdmin, By: a.user(req), At: stamp(time.Now())}
	if err := a.r.addAdminNote(ctx, agent, id, n); err != nil {
		a.fail(w, "add the note", err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, n)
}

func (a *consoleAPI) deleteNote(w http.ResponseWriter, agent, id, noteID string) {
	if !noteIDRe.MatchString(noteID) {
		writeError(w, http.StatusBadRequest, "invalid note id")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), viewTimeout)
	defer cancel()
	if !a.exists(ctx, w, agent, id, "delete the note") {
		return
	}
	removed, err := a.r.removeNote(ctx, agent, id, noteID)
	switch {
	case err != nil:
		a.fail(w, "delete the note", err)
	case !removed:
		writeError(w, http.StatusNotFound, "note not found")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
