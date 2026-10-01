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
	"regexp"
	"strings"
	"time"

	"github.com/justmike1/arbetern/github"
	"github.com/justmike1/arbetern/internal/httpx"
	"github.com/justmike1/arbetern/internal/redact"
	"github.com/justmike1/arbetern/internal/sessiontoken"
	"github.com/justmike1/arbetern/internal/store"
	"github.com/justmike1/arbetern/internal/text"
)

const gatewayTimeout = 45 * time.Second

var exitReasonRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// TokenVerifier checks the session token a runner presents.
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (*sessiontoken.Claims, error)
}

type gateway struct {
	r         *Registry
	verifier  TokenVerifier
	accountID string
	resolve   func(ctx context.Context, claims *sessiontoken.Claims) (*session, int, error)
}

type session struct {
	claims  *sessiontoken.Claims
	project *Project
	ledger  *Ledger
	task    *Task
}

type sessionKey struct{}

// NewGateway returns the runner-facing API; it authenticates with session tokens only.
func NewGateway(r *Registry, v TokenVerifier, accountID string) http.Handler {
	g := &gateway{r: r, verifier: v, accountID: strings.TrimSpace(accountID)}
	g.resolve = g.resolveSession
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/session", g.handleSession)
	mux.HandleFunc("POST /v1/session/ended", g.handleEnded)
	mux.HandleFunc("POST /mcp", g.handleMCP)
	mux.HandleFunc("GET /mcp", mcpOnlyPost)
	mux.HandleFunc("DELETE /mcp", mcpOnlyPost)
	return g.authenticate(mux)
}

func mcpOnlyPost(w http.ResponseWriter, _ *http.Request) { methodNotAllowed(w, http.MethodPost) }

func bearerToken(h http.Header) (string, bool) {
	vals := h.Values("Authorization")
	if len(vals) != 1 {
		return "", false
	}
	scheme, token, ok := strings.Cut(strings.TrimSpace(vals[0]), " ")
	token = strings.TrimSpace(token)
	return token, ok && strings.EqualFold(scheme, "Bearer") && token != ""
}

func (g *gateway) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		for _, h := range []string{"Origin", "Sec-Fetch-Site", "Cookie"} {
			if _, browser := req.Header[h]; browser {
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
		}
		token, ok := bearerToken(req.Header)
		if !ok {
			log.Printf("[projects] gateway: %s %q from %s has no bearer token", req.Method, req.URL.Path, req.RemoteAddr)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), gatewayTimeout)
		defer cancel()
		claims, err := g.verifier.Verify(ctx, token)
		if errors.Is(err, sessiontoken.ErrUnavailable) {
			log.Printf("[projects] gateway: cannot verify session tokens yet: %v", err)
			writeError(w, http.StatusServiceUnavailable, "temporarily unavailable")
			return
		}
		if err != nil || claims == nil {
			log.Printf("[projects] gateway: rejected a session token from %s: %v", req.RemoteAddr, err)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if g.accountID != "" && claims.AccountID != g.accountID {
			log.Printf("[projects] gateway: session %s was not started by the project automation account", claims.SessionID)
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}
		s, status, err := g.resolve(ctx, claims)
		if err != nil {
			log.Printf("[projects] gateway: session %s: %v", claims.SessionID, err)
			msg := "forbidden"
			if status != http.StatusForbidden {
				msg = "temporarily unavailable"
			}
			writeError(w, status, msg)
			return
		}
		next.ServeHTTP(w, req.WithContext(context.WithValue(ctx, sessionKey{}, s)))
	})
}

func (g *gateway) resolveSession(ctx context.Context, claims *sessiontoken.Claims) (*session, int, error) {
	sk := claims.SessionID
	if !sessionKeyRe.MatchString(sk) {
		return nil, http.StatusForbidden, errors.New("the token carries no usable session id")
	}
	b, err := g.lookupBinding(ctx, sk)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, http.StatusForbidden, errors.New("no project dispatched this session")
	case err != nil:
		return nil, http.StatusServiceUnavailable, fmt.Errorf("reading the session binding: %w", err)
	}
	if !store.AgentRe.MatchString(b.Agent) || !store.IDRe.MatchString(b.Project) {
		return nil, http.StatusForbidden, errors.New("the session binding is malformed")
	}
	p, err := g.r.project(ctx, b.Agent, b.Project)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, http.StatusForbidden, fmt.Errorf("project %s/%s no longer exists", b.Agent, b.Project)
	case err != nil:
		return nil, http.StatusServiceUnavailable, fmt.Errorf("reading project %s/%s: %w", b.Agent, b.Project, err)
	}
	led, err := g.r.readLedger(ctx, b.Agent, b.Project)
	if err != nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("reading the ledger of %s/%s: %w", b.Agent, b.Project, err)
	}
	t := led.task(b.Task)
	if t == nil {
		return nil, http.StatusForbidden, fmt.Errorf("task %s is no longer in the ledger of %s/%s", b.Task, b.Agent, b.Project)
	}
	if sessiontoken.NormalizeSessionID(t.SessionID) != sk {
		return nil, http.StatusForbidden, fmt.Errorf("task %s belongs to another session", t.ID)
	}
	return &session{claims: claims, project: p, ledger: led, task: t}, http.StatusOK, nil
}

// bindingWaits covers a session that calls in before the tick that fired it has stored its binding.
var bindingWaits = []time.Duration{time.Second, 2 * time.Second}

func (g *gateway) lookupBinding(ctx context.Context, sk string) (*binding, error) {
	for i := 0; ; i++ {
		b, _, err := store.GetJSON[binding](ctx, g.r.b, bindingKey(sk))
		if !errors.Is(err, store.ErrNotFound) || i == len(bindingWaits) {
			return b, err
		}
		if httpx.SleepCtx(ctx, bindingWaits[i]) != nil {
			return nil, store.ErrNotFound
		}
	}
}

func sessionFrom(req *http.Request) *session {
	s, _ := req.Context().Value(sessionKey{}).(*session)
	return s
}

func readBody(w http.ResponseWriter, req *http.Request) ([]byte, int, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxBodyBytes))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		return nil, http.StatusRequestEntityTooLarge, errors.New("request body too large")
	case err != nil:
		return nil, http.StatusBadRequest, errors.New("could not read the request body")
	}
	return body, 0, nil
}

func (g *gateway) handleSession(w http.ResponseWriter, req *http.Request) {
	s := sessionFrom(req)
	if isTerminal(s.task.Status) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"project":  s.project.Agent + "/" + s.project.ID,
		"task":     s.task.ID,
		"status":   s.task.Status,
		"reported": s.task.Outcome != nil,
	})
}

func exitReason(v string) string {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return ""
	case exitReasonRe.MatchString(v):
		return strings.ToLower(v)
	}
	return "unknown"
}

func (g *gateway) handleEnded(w http.ResponseWriter, req *http.Request) {
	s := sessionFrom(req)
	body, status, err := readBody(w, req)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	var in struct {
		ExitReason string `json:"exit_reason"`
		Branches   []struct {
			Name string `json:"name"`
			Head string `json:"head"`
		} `json:"branches"`
	}
	if len(bytes.TrimSpace(body)) > 0 && json.Unmarshal(body, &in) != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	reason := exitReason(in.ExitReason)
	branch := ""
	for _, b := range in.Branches {
		if name := strings.TrimSpace(b.Name); strings.HasPrefix(name, BranchPrefix) && validBranchName(name) {
			branch = name
			break
		}
	}
	now := time.Now().UTC()
	taskID := s.task.ID
	changed, err := g.apply(req.Context(), s, func(l *Ledger) bool { return l.sessionEnded(taskID, reason, branch, now) })
	if err != nil {
		log.Printf("[projects] gateway: recording the end of session %s: %v", s.claims.SessionID, err)
		writeError(w, http.StatusServiceUnavailable, "temporarily unavailable")
		return
	}
	log.Printf("[projects] %s/%s task %s: session ended (exit %s, status %s, account %s)", s.project.Agent, s.project.ID, taskID, reason, s.task.Status, s.claims.AccountID)
	if changed {
		g.r.refreshStats(s.project.Agent, s.project.ID, s.ledger)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (g *gateway) handleMCP(w http.ResponseWriter, req *http.Request) {
	s := sessionFrom(req)
	body, status, err := readBody(w, req)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	code, resp := serveRPC(req.Context(), body, g.tools(s))
	if code == http.StatusAccepted {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(resp)
}

func (g *gateway) tools(s *session) toolCaller {
	return func(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
		switch name {
		case "get_task":
			return g.getTask(ctx, s, args)
		case "report_outcome":
			return g.reportOutcome(ctx, s, args)
		case "record_learning":
			return g.recordLearning(ctx, s, args)
		}
		return "", false, fmt.Errorf("no handler for tool %q", name)
	}
}

// apply keeps s current, so later calls in the same MCP batch see the transition.
func (g *gateway) apply(ctx context.Context, s *session, fn func(*Ledger) bool) (bool, error) {
	led, changed, err := g.r.mergeLedger(ctx, s.project.Agent, s.project.ID, s.ledger, fn)
	if err != nil {
		return false, err
	}
	s.ledger = led
	if t := led.task(s.task.ID); t != nil {
		s.task = t
	}
	return changed, nil
}

func (g *gateway) getTask(ctx context.Context, s *session, raw json.RawMessage) (string, bool, error) {
	if err := decodeStrict(raw, &struct{}{}); err != nil {
		return "get_task takes no arguments.", true, nil
	}
	mem, err := g.r.readMemory(ctx, s.project.Agent, s.project.ID)
	if err != nil {
		return "", false, fmt.Errorf("reading project memory: %w", err)
	}
	recent := s.ledger.recentOutcomes(s.task.ID, recentOutcomeCount)
	return renderBrief(s.project, s.task, mem.Notes, recent, g.r.skillsFor(s.project.Agent)), false, nil
}

type reportArgs struct {
	Status  string `json:"status"`
	Summary string `json:"summary"`
	Title   string `json:"title"`
	Branch  string `json:"branch"`
	Testing string `json:"testing"`
}

func (a reportArgs) outcome(base string, now time.Time) (o Outcome, problem string) {
	o = Outcome{
		Status:     strings.TrimSpace(a.Status),
		Summary:    strings.TrimSpace(stripControl(a.Summary)),
		Testing:    strings.TrimSpace(stripControl(a.Testing)),
		Title:      oneLine(a.Title, 4*120),
		ReportedAt: stamp(now),
	}
	switch o.Status {
	case OutcomeFixed, OutcomeNotReproducible, OutcomeCannotFix, OutcomeAlreadyFixed:
	default:
		return o, "status must be one of fixed, not_reproducible, cannot_fix or already_fixed."
	}
	for _, c := range []error{
		checkText("summary", o.Summary, 1, 4000),
		checkText("testing", o.Testing, 0, 2000),
		checkText("title", o.Title, 0, 120),
	} {
		if c != nil {
			return o, c.Error() + "."
		}
	}
	if o.Status != OutcomeFixed {
		return o, ""
	}
	branch := strings.TrimSpace(a.Branch)
	switch {
	case o.Title == "":
		return o, "title is required when status is fixed."
	case branch == "":
		return o, "branch is required when status is fixed: push your work and pass the branch name."
	case !strings.HasPrefix(branch, BranchPrefix) || !validBranchName(branch):
		return o, fmt.Sprintf("branch %q is not a valid %s branch name.", oneLine(branch, 200), BranchPrefix)
	case branch == base:
		return o, "branch must not be the base branch."
	}
	o.Branch = branch
	return o, ""
}

func reported(t *Task) (string, bool) {
	switch {
	case t.PR != nil:
		return "Opened pull request " + t.PR.URL + ". You can stop now.", true
	case t.Outcome != nil:
		return "An outcome (" + t.Outcome.Status + ") was already recorded for this work order. You can stop now.", true
	}
	return "", false
}

func (g *gateway) reportOutcome(ctx context.Context, s *session, raw json.RawMessage) (string, bool, error) {
	var a reportArgs
	if err := decodeStrict(raw, &a); err != nil {
		return "Invalid arguments: " + oneLine(err.Error(), 200), true, nil
	}
	now := time.Now().UTC()
	o, problem := a.outcome(s.project.Repo.BaseBranch, now)
	if problem != "" {
		return problem, true, nil
	}
	if reply, done := reported(s.task); done {
		return reply, false, nil
	}
	switch {
	case isTerminal(s.task.Status):
		return "This work order is already finished (" + s.task.Status + ").", true, nil
	case s.task.Status != StatusRunning:
		return "This work order is not running.", true, nil
	case o.Status == OutcomeFixed:
		return g.openPullRequest(ctx, s, o, now)
	}
	taskID, limits := s.task.ID, s.project.Limits.effective()
	changed, err := g.apply(ctx, s, func(l *Ledger) bool { return l.reportNoFix(taskID, o, limits, now) })
	if err != nil {
		return "", false, err
	}
	if !changed {
		if reply, done := reported(s.task); done {
			return reply, false, nil
		}
		return "A pull request is being opened for this work order; call report_outcome again in a minute.", true, nil
	}
	log.Printf("[projects] %s/%s task %s reported %s", s.project.Agent, s.project.ID, taskID, o.Status)
	g.r.refreshStats(s.project.Agent, s.project.ID, s.ledger)
	return "Recorded. You can stop now.", false, nil
}

func (g *gateway) openPullRequest(ctx context.Context, s *session, o Outcome, now time.Time) (string, bool, error) {
	p, taskID := s.project, s.task.ID
	guard := now.Format(time.RFC3339Nano)
	if _, err := g.apply(ctx, s, func(l *Ledger) bool { return l.beginOpening(taskID, guard, now) }); err != nil {
		return "", false, err
	}
	if reply, done := reported(s.task); done {
		return reply, false, nil
	}
	switch {
	case s.task.Opening == guard:
	case s.task.OpenAttempts >= maxOpenAttempts && !openingFresh(s.task, now):
		return fmt.Sprintf("report_outcome with status fixed already failed %d times for this work order; report cannot_fix with what you found instead.", maxOpenAttempts), true, nil
	default:
		return "A pull request is already being opened for this work order; call report_outcome again in a minute if it does not appear.", true, nil
	}
	url, problem := g.pullRequestFor(ctx, s, o)
	if problem != "" {
		g.releaseOpening(s, guard)
		return problem, true, nil
	}
	_, _, number, err := github.ParsePRURL(url)
	if err != nil {
		g.releaseOpening(s, guard)
		return "", false, fmt.Errorf("GitHub returned an unexpected pull request URL: %w", err)
	}
	opened := time.Now().UTC()
	pr := PR{Number: number, URL: url, Title: prTitle(p.Agent, o.Title), State: prStateOpen, CreatedAt: stamp(opened)}
	changed, err := g.apply(ctx, s, func(l *Ledger) bool { return l.openPR(taskID, o, pr, opened) })
	if err != nil {
		g.releaseOpening(s, guard)
		log.Printf("[projects] %s/%s task %s: recording pull request %s: %v", p.Agent, p.ID, taskID, url, err)
		return "Opened pull request " + url + " but could not record it; call report_outcome again with the same arguments.", true, nil
	}
	if !changed {
		if reply, done := reported(s.task); done {
			return reply, false, nil
		}
		return "The work order changed while the pull request was being opened (it is now " + s.task.Status + ").", true, nil
	}
	log.Printf("[projects] %s/%s task %s opened %s", p.Agent, p.ID, taskID, url)
	g.r.refreshStats(p.Agent, p.ID, s.ledger)
	return "Opened pull request " + url + ". You can stop now.", false, nil
}

func (g *gateway) releaseOpening(s *session, guard string) {
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	taskID := s.task.ID
	if _, err := g.apply(ctx, s, func(l *Ledger) bool { return l.clearOpening(taskID, guard) }); err != nil {
		log.Printf("[projects] %s/%s task %s: clearing the pull request guard: %v", s.project.Agent, s.project.ID, taskID, err)
	}
}

func githubProblem(err error) string {
	return oneLine(redact.Text(err.Error()), 300)
}

func (g *gateway) pullRequestFor(ctx context.Context, s *session, o Outcome) (url, problem string) {
	gh := g.r.gh
	if gh == nil {
		return "", "GitHub is not configured in this deployment, so no pull request can be opened."
	}
	p := s.project
	owner, repo, base := p.Repo.Owner, p.Repo.Name, p.Repo.BaseBranch
	exists, err := gh.BranchExists(ctx, owner, repo, o.Branch)
	if err != nil {
		return "", "Could not check branch " + o.Branch + " on GitHub (" + githubProblem(err) + "); call report_outcome again in a minute."
	}
	if !exists {
		return "", "Branch " + o.Branch + " is not on GitHub yet — push it and call report_outcome again."
	}
	ahead, err := gh.CommitsAhead(ctx, owner, repo, base, o.Branch)
	if err != nil {
		return "", "Could not compare " + o.Branch + " with " + base + " on GitHub (" + githubProblem(err) + "); call report_outcome again in a minute."
	}
	if ahead == 0 {
		return "", "Branch " + o.Branch + " has no commits ahead of base " + base + " — commit and push your change, then call report_outcome again."
	}
	existing, err := gh.FindOpenPullRequestByHead(ctx, owner, repo, o.Branch)
	if err != nil {
		return "", "Could not look for an open pull request on GitHub (" + githubProblem(err) + "); call report_outcome again in a minute."
	}
	if existing != nil && existing.URL != "" {
		return existing.URL, ""
	}
	url, err = gh.CreatePullRequest(ctx, owner, repo, base, o.Branch, prTitle(p.Agent, o.Title), prBody(p, s.task, o))
	if err != nil {
		return "", "GitHub did not open the pull request (" + githubProblem(err) + ")."
	}
	return url, ""
}

func (g *gateway) recordLearning(ctx context.Context, s *session, raw json.RawMessage) (string, bool, error) {
	var a struct {
		Note string `json:"note"`
	}
	if err := decodeStrict(raw, &a); err != nil {
		return "Invalid arguments: " + oneLine(err.Error(), 200), true, nil
	}
	note := oneLine(a.Note, 4*maxNoteChars)
	if err := checkText("note", note, 1, maxNoteChars); err != nil {
		return err.Error() + ".", true, nil
	}
	note = text.Truncate(redact.Text(note), maxNoteChars)
	switch {
	case isTerminal(s.task.Status):
		return "This work order is already finished (" + s.task.Status + ").", true, nil
	case s.task.Learnings >= maxLearnings:
		return fmt.Sprintf("You already recorded %d learnings for this work order.", maxLearnings), true, nil
	}
	id, err := store.NewID()
	if err != nil {
		return "", false, err
	}
	taskID := s.task.ID
	n := Note{ID: id, Text: note, Source: noteSession, Task: taskID, At: stamp(time.Now())}
	count := 0
	_, added, err := g.r.mergeMemory(ctx, s.project.Agent, s.project.ID, func(m *Memory) bool {
		count = m.count(noteSession, taskID)
		return count < maxLearnings && m.add(n)
	})
	if err != nil {
		return "", false, err
	}
	if !added {
		if count >= maxLearnings {
			return fmt.Sprintf("You already recorded %d learnings for this work order.", maxLearnings), true, nil
		}
		return "Project memory is full of admin notes; the note was not saved.", true, nil
	}
	if _, err := g.apply(ctx, s, func(l *Ledger) bool { return l.setLearnings(taskID, count+1) }); err != nil {
		log.Printf("[projects] %s/%s task %s: counting a learning: %v", s.project.Agent, s.project.ID, taskID, err)
	}
	return "Saved.", false, nil
}
