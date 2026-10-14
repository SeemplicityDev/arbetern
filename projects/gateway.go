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
	"net/http/httputil"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/github"
	"github.com/justmike1/arbetern/internal/httpx"
	"github.com/justmike1/arbetern/internal/redact"
	"github.com/justmike1/arbetern/internal/safego"
	"github.com/justmike1/arbetern/internal/store"
	"github.com/justmike1/arbetern/internal/text"
)

const (
	gatewayTimeout  = 45 * time.Second
	transferTimeout = 30 * time.Minute
	sessionCacheTTL = 15 * time.Second
	maxCachedTokens = 4096
	sessionWrapUp   = 10 * time.Minute
)

var (
	exitReasonRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	modelProbePaths = []string{modelPathPrefix, modelPathPrefix + "/", modelPathPrefix + "/api/hello"}
)

type gateway struct {
	r       *Registry
	resolve func(ctx context.Context, id, token string) (*session, int, error)
	models  *httputil.ReverseProxy
	git     *httputil.ReverseProxy
	tools   []toolDef
	cache   sessionCache
}

type session struct {
	id      string
	project *Project
	ledger  *Ledger
	task    *Task
	roundN  int
}

func (s *session) round() *Round {
	if s.roundN == 0 || s.task.Review == nil {
		return nil
	}
	return s.task.Review.round(s.roundN)
}

func (s *session) learningKey() string {
	if s.roundN > 0 {
		return s.task.ID + "#" + strconv.Itoa(s.roundN)
	}
	return s.task.ID
}

// live reports whether the session's work order still runs, or finished only moments ago and the session is wrapping up.
func (s *session) live() bool {
	finished, running := sessionFinished(s.task, s.roundN)
	return running || time.Since(parseTime(finished)) < sessionWrapUp
}

type cachedSession struct {
	s     *session
	until time.Time
}

type sessionCache struct {
	mu sync.Mutex
	m  map[string]cachedSession
}

func (c *sessionCache) get(key string) *session {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[key]; ok && time.Now().Before(e.until) {
		return e.s
	}
	return nil
}

func (c *sessionCache) put(key string, s *session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.m == nil || len(c.m) >= maxCachedTokens {
		fresh := map[string]cachedSession{}
		for k, e := range c.m {
			if now.Before(e.until) {
				fresh[k] = e
			}
		}
		c.m = fresh
	}
	if len(c.m) < maxCachedTokens {
		c.m[key] = cachedSession{s: s, until: now.Add(sessionCacheTTL)}
	}
}

type sessionKey struct{}

// NewGateway returns the session-facing API; it authenticates with the tokens arbetern issues to its own Jobs.
func NewGateway(r *Registry) http.Handler {
	g := &gateway{r: r, tools: workOrderTools}
	g.resolve = g.resolveSession
	if key := r.cluster.cfg.APIKey; key != "" {
		g.models = newModelProxy(anthropicAPI, key, r.usage)
	}
	if token := r.cluster.cfg.GitHubToken; token != "" {
		g.git = newGitProxy(githubWeb, token)
	}
	if r.deps.Tools != nil {
		g.tools = append(slices.Clone(workOrderTools), integrationTools(r.deps.Tools.Tools())...)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/session", g.handleSession)
	mux.HandleFunc("POST /v1/session/ended", g.handleEnded)
	mux.HandleFunc("GET /v1/session/transcript", g.handleGetTranscript)
	mux.HandleFunc("PUT /v1/session/transcript", g.handlePutTranscript)
	mux.HandleFunc("POST /mcp", g.handleMCP)
	mux.HandleFunc("GET /mcp", mcpOnlyPost)
	mux.HandleFunc("DELETE /mcp", mcpOnlyPost)
	mux.HandleFunc("POST /anthropic/v1/messages", g.handleModel)
	mux.HandleFunc("POST /anthropic/v1/messages/count_tokens", g.handleModel)
	mux.HandleFunc("GET /anthropic/v1/models", g.handleModel)
	mux.HandleFunc("GET /anthropic/v1/models/{model}", g.handleModel)
	mux.HandleFunc("GET /git/{owner}/{repo}/info/refs", g.handleGit)
	mux.HandleFunc("POST /git/{owner}/{repo}/git-upload-pack", g.handleGit)
	mux.HandleFunc("POST /git/{owner}/{repo}/git-receive-pack", g.handleGit)
	return g.authenticate(mux)
}

func mcpOnlyPost(w http.ResponseWriter, _ *http.Request) { methodNotAllowed(w, http.MethodPost) }

// sessionToken reads the token from the Bearer header, or from X-Api-Key, which is how Claude Code sends an API key.
func sessionToken(h http.Header) (string, bool) {
	if vals := h.Values("Authorization"); len(vals) == 1 {
		scheme, token, ok := strings.Cut(strings.TrimSpace(vals[0]), " ")
		token = strings.TrimSpace(token)
		if ok && strings.EqualFold(scheme, "Bearer") && token != "" {
			return token, true
		}
	}
	if vals := h.Values("X-Api-Key"); len(vals) == 1 && strings.TrimSpace(vals[0]) != "" {
		return strings.TrimSpace(vals[0]), true
	}
	return "", false
}

func (g *gateway) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if req.Method == http.MethodHead && slices.Contains(modelProbePaths, req.URL.Path) {
			// Claude Code probes its API base URL before it has sent any credential; the answer carries nothing.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		for _, h := range []string{"Origin", "Sec-Fetch-Site", "Cookie"} {
			if _, browser := req.Header[h]; browser {
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
		}
		token, ok := sessionToken(req.Header)
		if !ok {
			log.Printf("[projects] gateway: %s %q from %s carries no session token", req.Method, req.URL.Path, req.RemoteAddr)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		proxied := strings.HasPrefix(req.URL.Path, modelPathPrefix+"/") || strings.HasPrefix(req.URL.Path, gitPathPrefix)
		var ctx context.Context
		var cancel context.CancelFunc
		if proxied || strings.HasSuffix(req.URL.Path, "/transcript") {
			// Model streams, clones and transcripts outlast the listener's timeouts; a client that leaves cancels the upstream call.
			ctx, cancel = context.WithTimeout(req.Context(), transferTimeout)
			rc := http.NewResponseController(w)
			_ = rc.SetReadDeadline(time.Now().Add(transferTimeout))
			_ = rc.SetWriteDeadline(time.Now().Add(transferTimeout))
		} else {
			ctx, cancel = context.WithTimeout(context.WithoutCancel(req.Context()), gatewayTimeout)
		}
		defer cancel()
		key := tokenHash(token)
		s := g.cache.get(key)
		if s == nil || !proxied {
			var status int
			var err error
			if s, status, err = g.authorize(ctx, token, req.RemoteAddr); err != nil {
				msg := "forbidden"
				switch status {
				case http.StatusUnauthorized:
					msg = "unauthorized"
				case http.StatusServiceUnavailable:
					msg = "temporarily unavailable"
				}
				writeError(w, status, msg)
				return
			}
			if proxied {
				g.cache.put(key, s)
			}
		}
		next.ServeHTTP(w, req.WithContext(context.WithValue(ctx, sessionKey{}, s)))
	})
}

func (g *gateway) authorize(ctx context.Context, token, remote string) (*session, int, error) {
	id, ok := localSessionID(token)
	if !ok {
		log.Printf("[projects] gateway: rejected a token from %s that arbetern did not issue", remote)
		return nil, http.StatusUnauthorized, errors.New("invalid session token")
	}
	s, status, err := g.resolve(ctx, id, token)
	if err != nil {
		log.Printf("[projects] gateway: session %s: %v", id, err)
		return nil, status, err
	}
	return s, http.StatusOK, nil
}

func (g *gateway) resolveSession(ctx context.Context, id, token string) (*session, int, error) {
	b, err := g.lookupBinding(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, http.StatusForbidden, errors.New("no project dispatched this session")
	case err != nil:
		return nil, http.StatusServiceUnavailable, fmt.Errorf("reading the session binding: %w", err)
	case !store.IDRe.MatchString(b.Project):
		return nil, http.StatusForbidden, errors.New("the session binding is malformed")
	case !tokenMatches(token, b.TokenHash):
		return nil, http.StatusUnauthorized, errors.New("the token does not belong to this session")
	}
	p, err := g.r.project(ctx, b.Project)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, http.StatusForbidden, fmt.Errorf("project %s no longer exists", b.Project)
	case err != nil:
		return nil, http.StatusServiceUnavailable, fmt.Errorf("reading project %s: %w", b.Project, err)
	}
	led, err := g.r.readLedger(ctx, b.Project)
	if err != nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("reading the ledger of %s: %w", b.Project, err)
	}
	t := led.task(b.Task)
	if t == nil {
		return nil, http.StatusForbidden, fmt.Errorf("task %s is no longer in the ledger of %s", b.Task, b.Project)
	}
	s := &session{id: id, project: p, ledger: led, task: t, roundN: b.Round}
	owner, status := t.SessionID, t.Status
	if b.Round > 0 {
		rd := s.round()
		if rd == nil {
			return nil, http.StatusForbidden, fmt.Errorf("round %d of task %s is no longer in the ledger", b.Round, t.ID)
		}
		owner, status = rd.SessionID, rd.Status
	}
	switch {
	case owner == id:
		return s, http.StatusOK, nil
	case owner != "" || status != StatusDispatching:
		return nil, http.StatusForbidden, fmt.Errorf("task %s belongs to another session", t.ID)
	}
	// The binding is stored before the Job starts and the tick names the session once it has. A pod that checks in first
	// completes the dispatch itself, so a tick lost between the two strands nothing.
	st, now := &started{SessionID: id, ClaudeSession: b.ClaudeSession, Branch: b.Branch}, time.Now().UTC()
	transition := func(l *Ledger) bool { return l.markRunning(t.ID, st, now) }
	if b.Round > 0 {
		transition = func(l *Ledger) bool { return l.roundRunning(t.ID, b.Round, st, now) }
	}
	if _, err := g.apply(ctx, s, transition); err != nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("recording the session of task %s: %w", t.ID, err)
	}
	owner = s.task.SessionID
	if b.Round > 0 {
		if rd := s.round(); rd != nil {
			owner = rd.SessionID
		}
	}
	if owner != id {
		return nil, http.StatusForbidden, fmt.Errorf("task %s belongs to another session", t.ID)
	}
	return s, http.StatusOK, nil
}

func (g *gateway) lookupBinding(ctx context.Context, id string) (*binding, error) {
	b, _, err := store.GetJSON[binding](ctx, g.r.b, bindingKey(id))
	return b, err
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

// handleSession admits a pod only while its work order runs; the Stop hook reads the same answer to let a reported session stop.
func (g *gateway) handleSession(w http.ResponseWriter, req *http.Request) {
	s := sessionFrom(req)
	if _, running := sessionFinished(s.task, s.roundN); !running {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	status := s.task.Status
	if rd := s.round(); rd != nil {
		status = rd.Status
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"project": s.project.ID, "task": s.task.ID, "round": s.roundN, "status": status, "reported": false})
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
		Turns      int    `json:"turns"`
		Branches   []struct {
			Name string `json:"name"`
		} `json:"branches"`
	}
	if len(bytes.TrimSpace(body)) > 0 && json.Unmarshal(body, &in) != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	reason, turns := exitReason(in.ExitReason), max(in.Turns, 0)
	branch := ""
	for _, b := range in.Branches {
		if name := strings.TrimSpace(b.Name); strings.HasPrefix(name, BranchPrefix) && validBranchName(name) && s.pushAllowed(name) == nil {
			branch = name
			break
		}
	}
	now := time.Now().UTC()
	taskID, n := s.task.ID, s.roundN
	transition := func(l *Ledger) bool { return l.sessionEnded(taskID, reason, branch, turns, now) }
	if n > 0 {
		transition = func(l *Ledger) bool { return l.roundEnded(taskID, n, reason, turns, now) }
	}
	changed, err := g.apply(req.Context(), s, transition)
	if err != nil {
		log.Printf("[projects] gateway: recording the end of session %s: %v", s.id, err)
		writeError(w, http.StatusServiceUnavailable, "temporarily unavailable")
		return
	}
	log.Printf("[projects] %s task %s round %d: session ended (exit %s, %d turns, status %s)", s.project.ID, taskID, n, reason, turns, s.task.Status)
	g.r.usage.flushSession(s.id)
	if changed {
		g.r.refreshStats(s.project.ID, s.ledger)
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
	code, resp := serveRPC(req.Context(), body, g.tools, g.toolCall(s))
	if code == http.StatusAccepted {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(resp)
}

func (g *gateway) toolCall(s *session) toolCaller {
	return func(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
		switch name {
		case "get_task":
			return g.getTask(ctx, s, args)
		case "report_outcome":
			return g.reportOutcome(ctx, s, args)
		case "record_learning":
			return g.recordLearning(ctx, s, args)
		}
		if g.r.deps.Tools == nil {
			return "", false, fmt.Errorf("no handler for tool %q", name)
		}
		if !s.live() {
			return "This work order is finished, so its tools are closed.", true, nil
		}
		out := g.r.deps.Tools.Call(ctx, "project:"+s.project.ID+"/"+s.task.ID, name, string(args))
		return out, strings.HasPrefix(out, "Error"), nil
	}
}

// apply keeps s current, so later calls in the same MCP batch see the transition.
func (g *gateway) apply(ctx context.Context, s *session, fn func(*Ledger) bool) (bool, error) {
	led, changed, err := g.r.mergeLedger(ctx, s.project.ID, s.ledger, fn)
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
	mem, err := g.r.readMemory(ctx, s.project.ID)
	if err != nil {
		return "", false, fmt.Errorf("reading project memory: %w", err)
	}
	skills := g.r.skillsFor(s.project.Skills)
	if s.roundN > 0 {
		rd := s.round()
		if rd == nil {
			return "This follow-up round is no longer in the ledger.", true, nil
		}
		return renderRoundBrief(s.project, s.task, rd, mem.Notes, skills), false, nil
	}
	recent := s.ledger.recentOutcomes(s.task.ID, recentOutcomeCount)
	inflight := s.ledger.inFlight(s.task.ID)
	return renderBrief(s.project, s.task, mem.Notes, recent, inflight, skills), false, nil
}

type replyArg struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

type reportArgs struct {
	Status      string     `json:"status"`
	Summary     string     `json:"summary"`
	Title       string     `json:"title"`
	Branch      string     `json:"branch"`
	Testing     string     `json:"testing"`
	Question    string     `json:"question"`
	Headline    string     `json:"headline"`
	Ticket      *ticketArg `json:"ticket"`
	Replies     []replyArg `json:"replies"`
	RerunChecks bool       `json:"rerun_checks"`
}

func (a reportArgs) common(now time.Time, maxSummary int) (o Outcome, problem string) {
	o = Outcome{
		Status:     strings.TrimSpace(a.Status),
		Summary:    strings.TrimSpace(stripControl(a.Summary)),
		Testing:    strings.TrimSpace(stripControl(a.Testing)),
		Title:      oneLine(a.Title, 4*120),
		Question:   strings.TrimSpace(stripControl(a.Question)),
		Headline:   oneLine(stripControl(a.Headline), 4*maxHeadlineChars),
		ReportedAt: stamp(now),
	}
	for _, c := range []error{
		checkText("summary", o.Summary, 1, maxSummary),
		checkText("testing", o.Testing, 0, 2000),
		checkText("title", o.Title, 0, 120),
		checkText("question", o.Question, 0, maxQuestionChars),
		checkText("headline", o.Headline, 0, maxHeadlineChars),
	} {
		if c != nil {
			return o, c.Error() + "."
		}
	}
	return o, ""
}

func checkPushBranch(branch, base string) (string, string) {
	branch = strings.TrimSpace(branch)
	switch {
	case branch == "":
		return "", "branch is required when status is fixed: push your work and pass the branch name."
	case !validBranchName(branch):
		return "", fmt.Sprintf("branch %q is not a valid branch name.", oneLine(branch, 200))
	case branch == base:
		return "", "branch must not be the base branch."
	}
	return branch, ""
}

func (a reportArgs) outcome(p *Project, now time.Time) (o Outcome, problem string) {
	maxSummary := 4000
	if p.Conventions.Description == descriptionSession {
		maxSummary = maxSessionSummary
	}
	o, problem = a.common(now, maxSummary)
	switch {
	case problem != "":
		return o, problem
	case len(a.Replies) > 0 || a.RerunChecks:
		return o, "replies and rerun_checks belong to follow-up rounds only."
	}
	switch o.Status {
	case OutcomeFixed, OutcomeNotReproducible, OutcomeCannotFix, OutcomeAlreadyFixed:
	default:
		return o, "status must be one of fixed, not_reproducible, cannot_fix or already_fixed."
	}
	if o.Status != OutcomeFixed {
		if o.Question != "" {
			return o, "question goes with status fixed: push your best option and ask about the alternatives."
		}
		return o, ""
	}
	if o.Title == "" {
		return o, "title is required when status is fixed."
	}
	if pattern := p.Conventions.TitlePattern; pattern != "" && !regexp.MustCompile(pattern).MatchString(o.Title) {
		return o, "title " + strconvQuote(o.Title) + " does not follow the repository's convention: it must match " + pattern + "."
	}
	o.Branch, problem = checkPushBranch(a.Branch, p.Repo.BaseBranch)
	return o, problem
}

func strconvQuote(s string) string { return strconv.Quote(oneLine(s, 200)) }

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
	if s.roundN > 0 {
		return g.reportRound(ctx, s, a)
	}
	if reply, done := reported(s.task); done {
		return reply, false, nil
	}
	now := time.Now().UTC()
	o, problem := a.outcome(s.project, now)
	var ticket *TicketRequest
	if problem == "" && o.Status == OutcomeFixed {
		if err := s.pushAllowed(o.Branch); err != nil {
			problem = "Report the branch your work order named: " + err.Error() + "."
		}
		if tc := s.project.Conventions.Ticket; problem == "" && tc != nil {
			var summary, description string
			var fields map[string]any
			if summary, description, fields, problem = a.Ticket.check(tc); problem == "" {
				ticket = &TicketRequest{Project: tc.Project, IssueType: tc.IssueType, Summary: summary, Description: description,
					AssigneeID: tc.AssigneeID, Labels: tc.Labels, Fields: ticketFields(tc, taskSite(s.project, s.task.Group), fields)}
			}
		}
	}
	if problem != "" {
		return problem, true, nil
	}
	switch {
	case isTerminal(s.task.Status):
		return "This work order is already finished (" + s.task.Status + ").", true, nil
	case s.task.Status != StatusRunning:
		return "This work order is not running.", true, nil
	case o.Status == OutcomeFixed:
		return g.openPullRequest(ctx, s, o, ticket, now)
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
	log.Printf("[projects] %s task %s reported %s", s.project.ID, taskID, o.Status)
	g.r.refreshStats(s.project.ID, s.ledger)
	g.r.notify(s.project, noteNoFix(s.task, o))
	return "Recorded. You can stop now.", false, nil
}

func (g *gateway) openPullRequest(ctx context.Context, s *session, o Outcome, ticket *TicketRequest, now time.Time) (string, bool, error) {
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
	url, branch, problem := g.pullRequestFor(ctx, s, o, ticket)
	if problem != "" {
		g.releaseOpening(s, guard)
		return problem, true, nil
	}
	sessionBranch := o.Branch
	o.Branch = branch
	_, _, number, err := github.ParsePRURL(url)
	if err != nil {
		g.releaseOpening(s, guard)
		return "", false, fmt.Errorf("GitHub returned an unexpected pull request URL: %w", err)
	}
	opened := time.Now().UTC()
	pr := PR{Number: number, URL: url, Title: prTitle(p, s.task, o.Title), State: prStateOpen, CreatedAt: stamp(opened)}
	follow, draft := p.Review.FollowUp, opensDraft(p, o)
	changed, err := g.apply(ctx, s, func(l *Ledger) bool { return l.openPR(taskID, o, pr, follow, draft, opened) })
	if err != nil {
		g.releaseOpening(s, guard)
		log.Printf("[projects] %s task %s: recording pull request %s: %v", p.ID, taskID, url, err)
		return "Opened pull request " + url + " but could not record it; call report_outcome again with the same arguments.", true, nil
	}
	if !changed {
		if reply, done := reported(s.task); done {
			return reply, false, nil
		}
		return "The work order changed while the pull request was being opened (it is now " + s.task.Status + ").", true, nil
	}
	log.Printf("[projects] %s task %s opened %s", p.ID, taskID, url)
	g.r.refreshStats(p.ID, s.ledger)
	g.r.notify(p, noteOpened(s.task, o))
	repo, key := p.Repo, s.task.Ticket
	safego.Go("projects: after opening "+taskID, func() { g.afterOpening(p.ID, repo, taskID, key, sessionBranch, branch, url) })
	return "Opened pull request " + url + ". You can stop now.", false, nil
}

// afterOpening links the pull request from its ticket and drops the session's own branch once the pull request has a branch of its own.
func (g *gateway) afterOpening(projectID string, repo Repo, taskID, ticket, sessionBranch, branch, url string) {
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	if ticket != "" && g.r.deps.Tickets != nil {
		if err := g.r.deps.Tickets.CommentTicket(ctx, ticket, "Pull request: "+url); err != nil {
			log.Printf("[projects] %s task %s: linking %s from %s: %v", projectID, taskID, url, ticket, err)
		}
	}
	if sessionBranch == branch || g.r.gh == nil {
		return
	}
	if other, err := g.r.gh.FindOpenPullRequestByHead(ctx, repo.Owner, repo.Name, sessionBranch); err == nil && other == nil {
		if err := g.r.gh.DeleteBranch(ctx, repo.Owner, repo.Name, sessionBranch); err != nil {
			log.Printf("[projects] %s task %s: deleting the session branch %s: %v", projectID, taskID, sessionBranch, err)
		}
	}
}

func opensDraft(p *Project, o Outcome) bool { return p.Review.Draft || o.Question != "" }

func (g *gateway) releaseOpening(s *session, guard string) {
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	taskID := s.task.ID
	if _, err := g.apply(ctx, s, func(l *Ledger) bool { return l.clearOpening(taskID, guard) }); err != nil {
		log.Printf("[projects] %s task %s: clearing the pull request guard: %v", s.project.ID, taskID, err)
	}
}

func githubProblem(err error) string {
	return oneLine(redact.Text(err.Error()), 300)
}

func (g *gateway) pullRequestFor(ctx context.Context, s *session, o Outcome, ticket *TicketRequest) (url, branch, problem string) {
	gh := g.r.gh
	if gh == nil {
		return "", "", "GitHub is not configured in this deployment, so no pull request can be opened."
	}
	p := s.project
	owner, repo, base := p.Repo.Owner, p.Repo.Name, p.Repo.BaseBranch
	exists, err := gh.BranchExists(ctx, owner, repo, o.Branch)
	if err != nil {
		return "", "", "Could not check branch " + o.Branch + " on GitHub (" + githubProblem(err) + "); call report_outcome again in a minute."
	}
	if !exists {
		return "", "", "Branch " + o.Branch + " is not on GitHub yet — push it and call report_outcome again."
	}
	ahead, err := gh.CommitsAhead(ctx, owner, repo, base, o.Branch)
	if err != nil {
		return "", "", "Could not compare " + o.Branch + " with " + base + " on GitHub (" + githubProblem(err) + "); call report_outcome again in a minute."
	}
	if ahead == 0 {
		return "", "", "Branch " + o.Branch + " has no commits ahead of base " + base + " — commit and push your change, then call report_outcome again."
	}
	if ticket != nil {
		if problem = g.fileTaskTicket(ctx, s, *ticket); problem != "" {
			return "", "", problem
		}
	}
	branch = prBranch(p, s.task, o)
	if branch != o.Branch {
		if problem = g.placeBranch(ctx, s, o.Branch, branch); problem != "" {
			return "", "", problem
		}
	}
	existing, err := gh.FindOpenPullRequestByHead(ctx, owner, repo, branch)
	if err != nil {
		return "", "", "Could not look for an open pull request on GitHub (" + githubProblem(err) + "); call report_outcome again in a minute."
	}
	if existing != nil && existing.URL != "" {
		return existing.URL, branch, ""
	}
	create := gh.CreatePullRequest
	if opensDraft(p, o) {
		create = gh.CreateDraftPullRequest
	}
	url, err = create(ctx, owner, repo, base, branch, prTitle(p, s.task, o.Title), prBody(p, s.task, o))
	if err != nil {
		return "", "", "GitHub did not open the pull request (" + githubProblem(err) + ")."
	}
	return url, branch, ""
}

// fileTaskTicket files the task's issue once; a report that is retried reuses the issue already recorded.
func (g *gateway) fileTaskTicket(ctx context.Context, s *session, req TicketRequest) string {
	if s.task.Ticket != "" {
		return ""
	}
	if g.r.deps.Tickets == nil {
		return "This deployment has no Jira connection, so the ticket this project files with every pull request cannot be created. Report cannot_fix and say so."
	}
	if req.Description == "" {
		req.Description = req.Summary
	}
	req.Description += "\n\nFiled by the arbetern project \"" + oneLine(s.project.Name, 80) + "\" (task " + s.task.ID + ")."
	key, url, dropped, err := fileTicket(ctx, g.r.deps.Tickets, req)
	if err != nil {
		log.Printf("[projects] %s task %s: filing the ticket: %v", s.project.ID, s.task.ID, err)
		return "Could not file the ticket in Jira (" + oneLine(redact.Text(err.Error()), 300) + "); call report_outcome again in a minute."
	}
	if len(dropped) > 0 {
		log.Printf("[projects] %s task %s: filed %s without the fields Jira refused: %s", s.project.ID, s.task.ID, key, strings.Join(dropped, ", "))
	}
	taskID := s.task.ID
	if _, err := g.apply(ctx, s, func(l *Ledger) bool { return l.setTicket(taskID, key, url) }); err != nil {
		log.Printf("[projects] %s task %s: recording ticket %s: %v", s.project.ID, taskID, key, err)
		return "Filed " + key + " but could not record it; call report_outcome again in a minute."
	}
	if s.task.Ticket != key {
		return "Another report filed the ticket for this work order first; call report_outcome again."
	}
	return ""
}

// placeBranch points the pull request branch the conventions name at the head the session pushed.
func (g *gateway) placeBranch(ctx context.Context, s *session, from, to string) string {
	p := s.project
	owner, repo := p.Repo.Owner, p.Repo.Name
	if !validBranchName(to) || to == p.Repo.BaseBranch {
		return "The pull request branch the conventions name, " + strconvQuote(to) + ", is not usable; adjust the title and call report_outcome again."
	}
	sha, err := g.r.gh.BranchHead(ctx, owner, repo, from)
	if err != nil {
		return "Could not read branch " + from + " on GitHub (" + githubProblem(err) + "); call report_outcome again in a minute."
	}
	current, err := g.r.gh.BranchHead(ctx, owner, repo, to)
	switch {
	case github.IsNotFound(err):
		if err := g.r.gh.CreateBranchAt(ctx, owner, repo, to, sha); err != nil {
			return "Could not create branch " + to + " on GitHub (" + githubProblem(err) + "); call report_outcome again in a minute."
		}
	case err != nil:
		return "Could not read branch " + to + " on GitHub (" + githubProblem(err) + "); call report_outcome again in a minute."
	case current != sha:
		if err := g.r.gh.FastForwardBranch(ctx, owner, repo, to, sha); err != nil {
			return "Could not move " + to + " to the head of " + from + " (" + githubProblem(err) + "); call report_outcome again in a minute."
		}
	}
	return ""
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
	learnings := s.task.Learnings
	if rd := s.round(); rd != nil {
		learnings = rd.Learnings
	}
	switch {
	case isTerminal(s.task.Status):
		return "This work order is already finished (" + s.task.Status + ").", true, nil
	case learnings >= maxLearnings:
		return fmt.Sprintf("You already recorded %d learnings for this work order.", maxLearnings), true, nil
	}
	id, err := store.NewID()
	if err != nil {
		return "", false, err
	}
	taskID, noteKey, roundN := s.task.ID, s.learningKey(), s.roundN
	n := Note{ID: id, Text: note, Source: noteSession, Task: noteKey, At: stamp(time.Now())}
	count := 0
	_, added, err := g.r.mergeMemory(ctx, s.project.ID, func(m *Memory) bool {
		count = m.count(noteSession, noteKey)
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
	if _, err := g.apply(ctx, s, func(l *Ledger) bool { return l.setLearnings(taskID, roundN, count+1) }); err != nil {
		log.Printf("[projects] %s task %s: counting a learning: %v", s.project.ID, taskID, err)
	}
	return "Saved.", false, nil
}
