package projects

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/billing"
)

var (
	anthropicAPI = &url.URL{Scheme: "https", Host: "api.anthropic.com"}
	githubWeb    = &url.URL{Scheme: "https", Host: "github.com"}
)

const (
	maxModelRequest  = 32 << 20
	maxPushRequest   = 512 << 20
	maxPushCommands  = 32
	maxPushHead      = 1 << 20
	maxPushHeadLines = 256
	maxSSELine       = 1 << 20
	maxUsageBody     = 8 << 20
	usageFlushEvery  = 5 * time.Minute
	upstreamTimeout  = 10 * time.Minute
	zeroOID          = "0000000000000000000000000000000000000000"
	sourceProject    = "project"
	billingAgent     = "projects"
	pushRefPrefix    = "refs/heads/" + BranchPrefix
	modelPathPrefix  = "/anthropic"
	gitPathPrefix    = "/git/"
	pktFlush         = "0000"
	maxPktLineLength = 65520
)

func upstreamTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = upstreamTimeout
	t.DialContext = (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return t
}

func stripCredentials(h http.Header) {
	for _, k := range []string{"Authorization", "X-Api-Key", "Cookie", "Proxy-Authorization"} {
		h.Del(k)
	}
}

func newModelProxy(upstream *url.URL, apiKey string, meter *usageMeter) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = &url.URL{Scheme: upstream.Scheme, Host: upstream.Host, Path: strings.TrimPrefix(pr.In.URL.Path, modelPathPrefix), RawQuery: pr.In.URL.RawQuery}
			pr.Out.Host = upstream.Host
			stripCredentials(pr.Out.Header)
			pr.Out.Header.Set("X-Api-Key", apiKey)
			// The transport then negotiates compression itself and hands the usage tap a plain body.
			pr.Out.Header.Del("Accept-Encoding")
		},
		FlushInterval: -1,
		Transport:     upstreamTransport(),
		ModifyResponse: func(resp *http.Response) error {
			s, _ := resp.Request.Context().Value(sessionKey{}).(*session)
			if s == nil || resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/v1/messages" {
				return nil
			}
			sse := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
			resp.Body = &usageTap{ReadCloser: resp.Body, sse: sse, done: func(model string, u apiUsage) {
				meter.add(usageKey{project: s.project.ID, name: s.project.Name, session: s.id, model: model}, u)
			}}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			log.Printf("[projects] model proxy: %v", err)
			writeError(w, http.StatusBadGateway, "the model API could not be reached")
		},
	}
}

func newGitProxy(upstream *url.URL, token string) *httputil.ReverseProxy {
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			target, _ := pr.In.Context().Value(gitTargetKey{}).(Repo)
			op := pr.In.URL.Path[strings.LastIndex(pr.In.URL.Path, "/"):]
			if strings.HasSuffix(pr.In.URL.Path, "/info/refs") {
				op = "/info/refs"
			}
			pr.Out.URL = &url.URL{Scheme: upstream.Scheme, Host: upstream.Host, Path: "/" + target.Owner + "/" + target.Name + ".git" + op, RawQuery: pr.In.URL.RawQuery}
			pr.Out.Host = upstream.Host
			stripCredentials(pr.Out.Header)
			pr.Out.Header.Set("Authorization", auth)
		},
		FlushInterval: -1,
		Transport:     upstreamTransport(),
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			log.Printf("[projects] git proxy: %v", err)
			http.Error(w, "GitHub could not be reached", http.StatusBadGateway)
		},
	}
}

func (g *gateway) handleModel(w http.ResponseWriter, req *http.Request) {
	s := sessionFrom(req)
	if !s.live() {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if g.models == nil {
		writeError(w, http.StatusServiceUnavailable, "model access is not configured")
		return
	}
	if m := req.PathValue("model"); m != "" && !modelRe.MatchString(m) {
		writeError(w, http.StatusBadRequest, "invalid model name")
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, maxModelRequest)
	g.models.ServeHTTP(w, req)
}

func (g *gateway) handleGit(w http.ResponseWriter, req *http.Request) {
	s := sessionFrom(req)
	if !s.live() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if g.git == nil {
		http.Error(w, "git access is not configured", http.StatusServiceUnavailable)
		return
	}
	owner, name := req.PathValue("owner"), strings.TrimSuffix(req.PathValue("repo"), ".git")
	target, status, err := g.gitTarget(req.Context(), s, owner, name)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	req = req.WithContext(context.WithValue(req.Context(), gitTargetKey{}, target))
	switch {
	case strings.HasSuffix(req.URL.Path, "/info/refs"):
		svc := req.URL.Query().Get("service")
		if svc != "git-upload-pack" && svc != "git-receive-pack" {
			http.Error(w, "only smart HTTP is supported", http.StatusForbidden)
			return
		}
		if svc == "git-receive-pack" {
			if status, err := g.pushRepo(req.Context(), s, target, false); err != nil {
				http.Error(w, err.Error(), status)
				return
			}
		}
	case strings.HasSuffix(req.URL.Path, "/git-receive-pack"):
		gzipped := strings.EqualFold(strings.TrimSpace(req.Header.Get("Content-Encoding")), "gzip")
		allowed := func(branch string) error {
			if branch == target.BaseBranch {
				return fmt.Errorf("sessions may not push to the base branch %s", branch)
			}
			return s.pushAllowed(branch)
		}
		body, err := checkPush(http.MaxBytesReader(w, req.Body, maxPushRequest), gzipped, allowed)
		if err != nil {
			log.Printf("[projects] %s task %s: refused a push: %v", s.project.ID, s.task.ID, err)
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		if status, err := g.pushRepo(req.Context(), s, target, true); err != nil {
			log.Printf("[projects] %s task %s: refused a push to %s: %v", s.project.ID, s.task.ID, target, err)
			http.Error(w, err.Error(), status)
			return
		}
		req.Body = body
		if gzipped {
			req.ContentLength = -1
			req.Header.Del("Content-Length")
			req.Header.Del("Content-Encoding")
		}
	}
	g.git.ServeHTTP(w, req)
}

type gitTargetKey struct{}

func (g *gateway) pushRepo(ctx context.Context, s *session, target Repo, bind bool) (int, error) {
	if !s.project.Repo.org() || strings.EqualFold(s.task.Repo, target.Name) {
		return 0, nil
	}
	// A work order pushes to one repository: the first push binds it on the stored ledger, so every replica refuses the others.
	taskID := s.task.ID
	led, err := g.r.readLedger(ctx, s.project.ID)
	if err == nil && bind {
		if t := led.task(taskID); t != nil && t.Repo == "" {
			led, _, err = g.r.mergeLedger(ctx, s.project.ID, nil, func(l *Ledger) bool { return l.bindRepo(taskID, target) })
		} else if t != nil && t.PR == nil && !strings.EqualFold(t.Repo, target.Name) && g.noBranchIn(ctx, s, t) {
			// The push that bound it never landed there, so the work order may still choose another repository.
			from := t.Repo
			led, _, err = g.r.mergeLedger(ctx, s.project.ID, nil, func(l *Ledger) bool { return l.rebindRepo(taskID, from, target) })
		}
	}
	if err != nil {
		log.Printf("[projects] %s task %s: git proxy: reading the ledger: %v", s.project.ID, taskID, err)
		return http.StatusBadGateway, errors.New("the push could not be checked; try again shortly")
	}
	switch t := led.task(taskID); {
	case t == nil:
		return http.StatusForbidden, errors.New("this work order is no longer in the ledger")
	case t.Repo == "" && (bind || isTerminal(t.Status)):
		return http.StatusForbidden, errors.New("this work order has finished, so nothing more can be pushed")
	case t.Repo == "":
		return 0, nil
	case !strings.EqualFold(t.Repo, target.Name):
		return http.StatusForbidden, fmt.Errorf("this work order already pushed to %s/%s; push only there", target.Owner, t.Repo)
	}
	return 0, nil
}

func (g *gateway) noBranchIn(ctx context.Context, s *session, t *Task) bool {
	if g.r.gh == nil {
		return false
	}
	branch := t.Branch
	if branch == "" {
		branch = taskBranch(s.project, t)
	}
	exists, err := g.r.gh.BranchExists(ctx, s.project.Repo.Owner, t.Repo, branch)
	return err == nil && !exists
}

func (g *gateway) gitTarget(ctx context.Context, s *session, owner, name string) (Repo, int, error) {
	if !ownerRe.MatchString(owner) || !repoNameRe.MatchString(name) || name == "." || name == ".." {
		return Repo{}, http.StatusBadRequest, errors.New("not a repository path")
	}
	repo := s.project.repoFor(s.task)
	if !strings.EqualFold(owner, repo.Owner) {
		return Repo{}, http.StatusForbidden, fmt.Errorf("this session may only reach repositories of %s", repo.Owner)
	}
	if repo.Name != "" && strings.EqualFold(name, repo.Name) {
		return repo, 0, nil
	}
	if !s.project.Repo.org() {
		return Repo{}, http.StatusForbidden, fmt.Errorf("this session may only reach %s/%s", repo.Owner, repo.Name)
	}
	found, ok, err := g.r.scopeRepo(ctx, s.project.Repo, name)
	switch {
	case err != nil:
		log.Printf("[projects] %s task %s: git proxy: %v", s.project.ID, s.task.ID, err)
		return Repo{}, http.StatusBadGateway, errors.New("the repositories this project may change could not be listed; try again shortly")
	case !ok:
		return Repo{}, http.StatusForbidden, fmt.Errorf("%s/%s is not one of the repositories this project may change", repo.Owner, name)
	}
	return Repo{Owner: repo.Owner, Name: found.Name, BaseBranch: found.DefaultBranch}, 0, nil
}

// pushAllowed limits a session to the branch its work order named and names derived from it: the task's branch from dispatch time,
// which becomes the pull request's branch once one is open, so a renamed project does not move it.
func (s *session) pushAllowed(branch string) error {
	own := s.task.Branch
	if own == "" {
		own = taskBranch(s.project, s.task)
	}
	if branch == own || strings.HasPrefix(branch, own+"-") {
		return nil
	}
	return fmt.Errorf("this session may push only %s or a name starting with %s-, not %s", own, own, branch)
}

func checkPush(body io.ReadCloser, gzipped bool, allowed func(branch string) error) (io.ReadCloser, error) {
	var r io.Reader = body
	if gzipped {
		zr, err := gzip.NewReader(body)
		if err != nil {
			return nil, fmt.Errorf("the push is not valid gzip: %w", err)
		}
		r = io.LimitReader(zr, maxPushRequest)
	}
	br := bufio.NewReader(r)
	var head bytes.Buffer
	commands, lines := 0, 0
	for {
		size := make([]byte, 4)
		if _, err := io.ReadFull(br, size); err != nil {
			return nil, errors.New("the push ended before its ref updates")
		}
		head.Write(size)
		if string(size) == pktFlush {
			break
		}
		if lines++; lines > maxPushHeadLines || head.Len() > maxPushHead {
			return nil, errors.New("the push lists too many refs")
		}
		n, err := strconv.ParseUint(string(size), 16, 16)
		if err != nil || n < 4 || n > maxPktLineLength {
			return nil, errors.New("the push has a malformed pkt-line")
		}
		line := make([]byte, n-4)
		if _, err := io.ReadFull(br, line); err != nil {
			return nil, errors.New("the push ended inside a ref update")
		}
		head.Write(line)
		cmd := strings.TrimSuffix(string(line), "\n")
		if i := strings.IndexByte(cmd, 0); i >= 0 {
			cmd = cmd[:i]
		}
		if strings.HasPrefix(cmd, "shallow ") {
			continue
		}
		if commands++; commands > maxPushCommands {
			return nil, fmt.Errorf("a push may update at most %d refs", maxPushCommands)
		}
		if err := checkRefUpdate(cmd, allowed); err != nil {
			return nil, err
		}
	}
	if commands == 0 {
		return nil, errors.New("the push updates no ref")
	}
	return readCloser{io.MultiReader(&head, br), body}, nil
}

func checkRefUpdate(cmd string, allowed func(branch string) error) error {
	fields := strings.Fields(cmd)
	switch {
	case strings.HasPrefix(cmd, "push-cert"):
		return errors.New("signed pushes are not supported")
	case len(fields) != 3:
		return errors.New("the push has a malformed ref update")
	case fields[1] == zeroOID:
		return fmt.Errorf("deleting %s is not allowed", fields[2])
	case !strings.HasPrefix(fields[2], "refs/heads/") || !validBranchName(strings.TrimPrefix(fields[2], "refs/heads/")):
		return fmt.Errorf("sessions may push only branches, not %s", fields[2])
	case allowed == nil && !strings.HasPrefix(fields[2], pushRefPrefix):
		return fmt.Errorf("sessions may push only %s branches, not %s", BranchPrefix, fields[2])
	case allowed == nil:
		return nil
	}
	return allowed(strings.TrimPrefix(fields[2], "refs/heads/"))
}

type readCloser struct {
	io.Reader
	io.Closer
}

type apiUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

func (u *apiUsage) merge(o apiUsage) {
	u.InputTokens = max(u.InputTokens, o.InputTokens)
	u.CacheCreationInputTokens = max(u.CacheCreationInputTokens, o.CacheCreationInputTokens)
	u.CacheReadInputTokens = max(u.CacheReadInputTokens, o.CacheReadInputTokens)
	u.OutputTokens = max(u.OutputTokens, o.OutputTokens)
}

type usageTap struct {
	io.ReadCloser
	sse   bool
	buf   []byte
	model string
	usage apiUsage
	once  sync.Once
	done  func(model string, u apiUsage)
}

func (t *usageTap) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	t.feed(p[:n])
	if err == io.EOF {
		t.finish()
	}
	return n, err
}

func (t *usageTap) Close() error {
	t.finish()
	return t.ReadCloser.Close()
}

func (t *usageTap) feed(p []byte) {
	if !t.sse {
		if len(t.buf)+len(p) <= maxUsageBody {
			t.buf = append(t.buf, p...)
		}
		return
	}
	t.buf = append(t.buf, p...)
	for {
		i := bytes.IndexByte(t.buf, '\n')
		if i < 0 {
			if len(t.buf) > maxSSELine {
				t.buf = t.buf[:0]
			}
			return
		}
		line := bytes.TrimSpace(t.buf[:i])
		t.buf = t.buf[i+1:]
		if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			t.event(bytes.TrimSpace(data))
		}
	}
}

func (t *usageTap) event(data []byte) {
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Model string   `json:"model"`
			Usage apiUsage `json:"usage"`
		} `json:"message"`
		Usage apiUsage `json:"usage"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		t.model = ev.Message.Model
		t.usage.merge(ev.Message.Usage)
	case "message_delta":
		t.usage.merge(ev.Usage)
	}
}

func (t *usageTap) finish() {
	t.once.Do(func() {
		if !t.sse {
			var msg struct {
				Model string   `json:"model"`
				Usage apiUsage `json:"usage"`
			}
			if json.Unmarshal(t.buf, &msg) == nil {
				t.model, t.usage = msg.Model, msg.Usage
			}
		}
		t.buf = nil
		if t.model != "" && t.usage != (apiUsage{}) {
			t.done(t.model, t.usage)
		}
	})
}

type usageKey struct {
	project, name, session, model string
}

type usageSum struct {
	apiUsage
	calls int
}

type usageMeter struct {
	mu     sync.Mutex
	sums   map[usageKey]*usageSum
	record func(billing.Event)
}

func newUsageMeter(record func(billing.Event)) *usageMeter {
	return &usageMeter{sums: map[usageKey]*usageSum{}, record: record}
}

func (m *usageMeter) add(k usageKey, u apiUsage) {
	if m == nil || m.record == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sum := m.sums[k]
	if sum == nil {
		sum = &usageSum{}
		m.sums[k] = sum
	}
	sum.calls++
	sum.InputTokens += u.InputTokens
	sum.CacheCreationInputTokens += u.CacheCreationInputTokens
	sum.CacheReadInputTokens += u.CacheReadInputTokens
	sum.OutputTokens += u.OutputTokens
}

func (m *usageMeter) flush(match func(usageKey) bool) {
	if m == nil || m.record == nil {
		return
	}
	m.mu.Lock()
	var events []billing.Event
	for k, u := range m.sums {
		if match != nil && !match(k) {
			continue
		}
		delete(m.sums, k)
		events = append(events, billing.Event{
			Agent:              billingAgent,
			Source:             sourceProject,
			WorkflowID:         k.project,
			WorkflowName:       k.name,
			Model:              k.model,
			Requests:           u.calls,
			PromptTokens:       u.InputTokens,
			CacheWriteTokens:   u.CacheCreationInputTokens,
			CachedPromptTokens: u.CacheReadInputTokens,
			CompletionTokens:   u.OutputTokens,
			TotalTokens:        u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens + u.OutputTokens,
		})
	}
	m.mu.Unlock()
	for _, e := range events {
		m.record(e)
	}
}

func (m *usageMeter) flushSession(id string) {
	m.flush(func(k usageKey) bool { return k.session == id })
}

func (m *usageMeter) run(ctx context.Context) {
	tick := time.NewTicker(usageFlushEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			m.flush(nil)
			return
		case <-tick.C:
			m.flush(nil)
		}
	}
}
