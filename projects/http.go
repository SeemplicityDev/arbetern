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

	"github.com/justmike1/arbetern/internal/httpx"
	"github.com/justmike1/arbetern/internal/store"
)

const apiTimeout = 30 * time.Second

type consoleAPI struct {
	r         *Registry
	authorize func(*http.Request) bool
	userFor   func(*http.Request) string
}

// RegisterRoutes mounts the projects console API; authorize gates every change and the session transcripts.
func (r *Registry) RegisterRoutes(apiMux *http.ServeMux, authorize func(*http.Request) bool, userFor func(*http.Request) string) {
	api := &consoleAPI{r: r, authorize: authorize, userFor: userFor}
	apiMux.HandleFunc("GET /api/projects", api.list)
	apiMux.HandleFunc("POST /api/projects", api.create)
	apiMux.HandleFunc("GET /api/projects/{id}", api.get)
	apiMux.HandleFunc("PATCH /api/projects/{id}", api.update)
	apiMux.HandleFunc("DELETE /api/projects/{id}", api.remove)
	apiMux.HandleFunc("POST /api/projects/{id}/run", api.run)
	apiMux.HandleFunc("POST /api/projects/{id}/memory", api.addNote)
	apiMux.HandleFunc("DELETE /api/projects/{id}/memory/{note}", api.deleteNote)
	apiMux.HandleFunc("GET /api/projects/{id}/transcripts/{task}", api.transcript)
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

// permit applies the admin list to a request that changes state or reads a transcript, and the same-origin check to the former.
func (a *consoleAPI) permit(w http.ResponseWriter, req *http.Request) bool {
	if req.Method != http.MethodGet {
		if err := httpx.CheckSameOrigin(req); err != nil {
			writeError(w, http.StatusForbidden, err.Error())
			return false
		}
	}
	if a.authorize != nil && !a.authorize(req) {
		writeError(w, http.StatusForbidden, "forbidden")
		return false
	}
	return true
}

func projectID(w http.ResponseWriter, req *http.Request) (string, bool) {
	id := req.PathValue("id")
	if !store.IDRe.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid project id")
		return "", false
	}
	return id, true
}

func (a *consoleAPI) exists(ctx context.Context, w http.ResponseWriter, id, action string) bool {
	_, err := a.r.project(ctx, id)
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
	case errors.Is(err, errBusy):
		writeError(w, http.StatusConflict, "a run of this project is in progress; try again in a moment")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "the project changed while it was being saved; try again")
	default:
		log.Printf("[projects] %s: %v", action, err)
		writeError(w, http.StatusInternalServerError, "could not "+action)
	}
}

func (a *consoleAPI) list(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, a.r.list())
}

func (a *consoleAPI) get(w http.ResponseWriter, req *http.Request) {
	id, ok := projectID(w, req)
	if !ok {
		return
	}
	v, ok := a.r.view(id)
	if !ok {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, v)
}

type createRequest struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Goal         string   `json:"goal"`
	Instructions string   `json:"instructions"`
	Skills       []string `json:"skills"`
	Repo         Repo     `json:"repo"`
	Signal       Signal   `json:"signal"`
	Session      Session  `json:"session"`
	Review       Review   `json:"review"`
	Notify       Notify   `json:"notify"`
	Limits       Limits   `json:"limits"`
	Interval     string   `json:"interval"`
	Enabled      *bool    `json:"enabled"`
}

func (a *consoleAPI) create(w http.ResponseWriter, req *http.Request) {
	if !a.permit(w, req) {
		return
	}
	var body createRequest
	if status, err := decodeBody(w, req, &body); err != nil {
		writeError(w, status, err.Error())
		return
	}
	draft := Project{
		Name:         body.Name,
		Description:  body.Description,
		Goal:         body.Goal,
		Instructions: body.Instructions,
		Skills:       body.Skills,
		Repo:         body.Repo,
		Signal:       body.Signal,
		Session:      body.Session,
		Review:       body.Review,
		Notify:       body.Notify,
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
	httpx.WriteJSON(w, http.StatusCreated, p)
}

func (a *consoleAPI) update(w http.ResponseWriter, req *http.Request) {
	id, ok := projectID(w, req)
	if !ok || !a.permit(w, req) {
		return
	}
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
		case "skills":
			pt.Skills = new([]string)
			if json.Unmarshal(v, pt.Skills) != nil {
				err = errors.New("must be a list of skill ids")
			}
		case "repo":
			pt.Repo = new(Repo)
			err = decodeStrict(v, pt.Repo)
		case "signal":
			pt.Signal = new(Signal)
			err = decodeStrict(v, pt.Signal)
		case "session":
			pt.Session = new(Session)
			err = decodeStrict(v, pt.Session)
		case "review":
			pt.Review = new(Review)
			err = decodeStrict(v, pt.Review)
		case "notify":
			pt.Notify = new(Notify)
			err = decodeStrict(v, pt.Notify)
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
	p, err := a.r.update(ctx, id, pt)
	if err != nil {
		a.fail(w, "update the project", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

func (a *consoleAPI) remove(w http.ResponseWriter, req *http.Request) {
	id, ok := projectID(w, req)
	if !ok || !a.permit(w, req) {
		return
	}
	if err := a.r.remove(id); err != nil {
		a.fail(w, "delete the project", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *consoleAPI) run(w http.ResponseWriter, req *http.Request) {
	id, ok := projectID(w, req)
	if !ok || !a.permit(w, req) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), viewTimeout)
	defer cancel()
	p, err := a.r.fetchProject(ctx, id)
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
	if err := a.r.trigger(ctx, id); err != nil {
		a.fail(w, "start a run", err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func (a *consoleAPI) addNote(w http.ResponseWriter, req *http.Request) {
	id, ok := projectID(w, req)
	if !ok || !a.permit(w, req) {
		return
	}
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
	if !a.exists(ctx, w, id, "add the note") {
		return
	}
	noteID, err := store.NewID()
	if err != nil {
		a.fail(w, "add the note", err)
		return
	}
	n := Note{ID: noteID, Text: note, Source: noteAdmin, By: a.user(req), At: stamp(time.Now())}
	if err := a.r.addAdminNote(ctx, id, n); err != nil {
		a.fail(w, "add the note", err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, n)
}

func (a *consoleAPI) deleteNote(w http.ResponseWriter, req *http.Request) {
	id, ok := projectID(w, req)
	if !ok || !a.permit(w, req) {
		return
	}
	noteID := req.PathValue("note")
	if !noteIDRe.MatchString(noteID) {
		writeError(w, http.StatusBadRequest, "invalid note id")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), viewTimeout)
	defer cancel()
	if !a.exists(ctx, w, id, "delete the note") {
		return
	}
	removed, err := a.r.removeNote(ctx, id, noteID)
	switch {
	case err != nil:
		a.fail(w, "delete the note", err)
	case !removed:
		writeError(w, http.StatusNotFound, "note not found")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *consoleAPI) transcript(w http.ResponseWriter, req *http.Request) {
	id, ok := projectID(w, req)
	if !ok {
		return
	}
	task := req.PathValue("task")
	if !store.IDRe.MatchString(task) {
		writeError(w, http.StatusBadRequest, "invalid task id")
		return
	}
	if !a.permit(w, req) {
		return
	}
	a.r.serveTranscript(w, req, id, task)
}
