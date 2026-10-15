package projects

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/justmike1/arbetern/internal/kube"
	"github.com/justmike1/arbetern/internal/store"
	"github.com/justmike1/arbetern/internal/ttlcache"
	"github.com/justmike1/arbetern/llm"
)

const (
	localSessionPrefix = "arb_"
	jobNamePrefix      = "claude-runner-"
	workOrderVolume    = "work-order"
	sessionLabel       = "arbetern.io/session-hash"
	taskLabel          = "arbetern.io/task"
	projectLabel       = "arbetern.io/project"
	sessionAnnotation  = "arbetern.io/session"
	templateCacheTTL   = time.Minute
	jobDeadlineGrace   = 10 * time.Minute
	kubeTimeout        = 20 * time.Second
)

// UserAgent identifies arbetern's session management to the Kubernetes API.
const UserAgent = "arbetern/projects"

// ClusterConfig is what arbetern needs to run project sessions as Jobs in its cluster.
type ClusterConfig struct {
	Kube        *kube.Client
	Namespace   string // where session Jobs run
	Template    string // file holding the session Job template as JSON
	GatewayURL  string // the gateway as session pods reach it
	APIKey      string // Anthropic API key the model proxy adds
	GitHubToken string // token the git proxy adds
	Model       string // model for sessions whose project names none
}

type cluster struct {
	cfg       ClusterConfig
	templates *ttlcache.Cache[[]byte]
}

func newCluster(cfg ClusterConfig) *cluster {
	c := &cluster{cfg: cfg}
	c.templates = ttlcache.New(templateCacheTTL, c.readTemplate)
	return c
}

func (c *cluster) readTemplate(context.Context) ([]byte, error) {
	raw, err := os.ReadFile(c.cfg.Template)
	if err != nil {
		return nil, fmt.Errorf("read the session Job template: %w", err)
	}
	return raw, nil
}

type clusterSession struct {
	id            string
	token         string
	project       *Project
	task          *Task
	round         int
	branch        string
	existing      bool // check out branch as it is on GitHub instead of creating it from the base branch
	claudeSession string
	resume        bool
	deadline      time.Duration
}

func (s clusterSession) workOrder() string {
	ref := s.project.ID + "/" + s.task.ID
	if s.round > 0 {
		ref += " round " + strconv.Itoa(s.round)
	}
	return ref
}

func (c *cluster) model(p *Project) string {
	m := strings.TrimSpace(p.Session.Model)
	if m == "" {
		m = c.cfg.Model
	}
	if m == "" {
		return ""
	}
	return llm.AnthropicModel(m)
}

func jobsPath(ns string) string { return "/apis/batch/v1/namespaces/" + url.PathEscape(ns) + "/jobs" }

func (c *cluster) start(ctx context.Context, s clusterSession) error {
	ctx, cancel := context.WithTimeout(ctx, kubeTimeout)
	defer cancel()
	raw, err := c.templates.Get(ctx)
	if err != nil {
		return err
	}
	name := jobNamePrefix + strings.ToLower(strings.TrimPrefix(s.id, localSessionPrefix))
	job, err := c.renderJob(raw, name, s)
	if err != nil {
		return err
	}
	code, body, err := c.cfg.Kube.Do(ctx, http.MethodPost, jobsPath(c.cfg.Namespace), job)
	switch {
	case err != nil:
		return fmt.Errorf("create job %s: %w", name, err)
	case code != http.StatusCreated && code != http.StatusOK:
		return kube.StatusError("create job "+name, code, body)
	}
	uid, ok := kube.UID(body)
	if !ok {
		return fmt.Errorf("create job %s: the response has no uid", name)
	}
	secret := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":   name,
			"labels": map[string]string{sessionLabel: sessionHash(s.id), taskLabel: s.task.ID, projectLabel: s.project.ID},
			"ownerReferences": []map[string]string{{
				"apiVersion": "batch/v1",
				"kind":       "Job",
				"name":       name,
				"uid":        uid,
			}},
		},
		"type":      "Opaque",
		"immutable": true,
		"data":      map[string]string{"token": base64.StdEncoding.EncodeToString([]byte(s.token))},
	}
	code, body, err = c.cfg.Kube.Do(ctx, http.MethodPost, "/api/v1/namespaces/"+url.PathEscape(c.cfg.Namespace)+"/secrets", secret)
	if err == nil && code != http.StatusCreated && code != http.StatusOK {
		err = kube.StatusError("create secret "+name, code, body, s.token)
	}
	if err != nil {
		c.deleteJob(context.WithoutCancel(ctx), name, uid)
		return fmt.Errorf("create the session secret: %w", err)
	}
	return nil
}

func (c *cluster) deleteJob(ctx context.Context, name, uid string) {
	ctx, cancel := context.WithTimeout(ctx, kubeTimeout)
	defer cancel()
	opts := map[string]any{
		"apiVersion":        "v1",
		"kind":              "DeleteOptions",
		"propagationPolicy": "Background",
	}
	if uid != "" {
		opts["preconditions"] = map[string]string{"uid": uid}
	}
	code, body, err := c.cfg.Kube.Do(ctx, http.MethodDelete, jobsPath(c.cfg.Namespace)+"/"+name, opts)
	switch {
	case err != nil:
		log.Printf("[projects] deleting job %s: %v", name, err)
	case code/100 != 2 && code != http.StatusNotFound:
		log.Print(kube.StatusError("delete job "+name, code, body))
	}
}

// stopSession deletes the Jobs of one session, so a task that timed out stops spending.
func (c *cluster) stopSession(ctx context.Context, sessionID string) {
	c.deleteBySelector(ctx, sessionLabel+"="+sessionHash(sessionID))
}

// stopTask deletes the Jobs of a task whose session was never recorded.
func (c *cluster) stopTask(ctx context.Context, taskID string) {
	c.deleteBySelector(ctx, taskLabel+"="+taskID)
}

// sessionJobs lists a project's session Jobs: the sessions that still have one, and those whose Job has finished.
func (c *cluster) sessionJobs(ctx context.Context, projectID string) (present, finished map[string]bool, ok bool) {
	if c == nil || c.cfg.Kube == nil {
		return nil, nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, kubeTimeout)
	defer cancel()
	code, body, err := c.cfg.Kube.Do(ctx, http.MethodGet, jobsPath(c.cfg.Namespace)+"?labelSelector="+url.QueryEscape(projectLabel+"="+projectID), nil)
	switch {
	case err != nil:
		log.Printf("[projects] %s listing session jobs: %v", projectID, err)
		return nil, nil, false
	case code != http.StatusOK:
		log.Print(kube.StatusError("list jobs of "+projectID, code, body))
		return nil, nil, false
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Status struct {
				Succeeded  int `json:"succeeded"`
				Failed     int `json:"failed"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		log.Printf("[projects] %s listing session jobs: %v", projectID, err)
		return nil, nil, false
	}
	present, finished = map[string]bool{}, map[string]bool{}
	for _, job := range list.Items {
		id := job.Metadata.Annotations[sessionAnnotation]
		if id == "" {
			continue
		}
		present[id] = true
		done := job.Status.Succeeded > 0 || job.Status.Failed > 0
		for _, cond := range job.Status.Conditions {
			if (cond.Type == "Complete" || cond.Type == "Failed") && cond.Status == "True" {
				done = true
			}
		}
		if done {
			finished[id] = true
		}
	}
	return present, finished, true
}

func (c *cluster) stopProject(ctx context.Context, projectID string) {
	c.deleteBySelector(ctx, projectLabel+"="+projectID)
}

func (c *cluster) deleteBySelector(ctx context.Context, selector string) {
	if c == nil || c.cfg.Kube == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, kubeTimeout)
	defer cancel()
	code, body, err := c.cfg.Kube.Do(ctx, http.MethodGet, jobsPath(c.cfg.Namespace)+"?labelSelector="+url.QueryEscape(selector), nil)
	switch {
	case err != nil:
		log.Printf("[projects] listing jobs %s: %v", selector, err)
		return
	case code != http.StatusOK:
		log.Print(kube.StatusError("list jobs "+selector, code, body))
		return
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
				UID  string `json:"uid"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		log.Printf("[projects] listing jobs %s: %v", selector, err)
		return
	}
	for _, job := range list.Items {
		c.deleteJob(ctx, job.Metadata.Name, job.Metadata.UID)
	}
}

func sessionHash(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:20]
}

func (c *cluster) renderJob(raw []byte, name string, s clusterSession) (map[string]any, error) {
	var job map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&job); err != nil || job == nil {
		return nil, fmt.Errorf("parse the session Job template: %v", err)
	}
	hash := sessionHash(s.id)
	for _, set := range []struct {
		path  string
		key   string
		value any
	}{
		{"metadata", "name", name},
		{"metadata", "namespace", c.cfg.Namespace},
		{"metadata.labels", sessionLabel, hash},
		{"metadata.labels", taskLabel, s.task.ID},
		{"metadata.labels", projectLabel, s.project.ID},
		{"metadata.annotations", sessionAnnotation, s.id},
		{"spec", "activeDeadlineSeconds", int64(s.deadline / time.Second)},
		{"spec.template.metadata.labels", sessionLabel, hash},
		{"spec.template.metadata.labels", taskLabel, s.task.ID},
		{"spec.template.metadata.labels", projectLabel, s.project.ID},
		{"spec.template.metadata.annotations", sessionAnnotation, s.id},
	} {
		obj, err := kube.Walk(job, set.path)
		if err != nil {
			return nil, fmt.Errorf("session Job template: %w", err)
		}
		obj[set.key] = set.value
	}
	pod, err := kube.Walk(job, "spec.template.spec")
	if err != nil {
		return nil, fmt.Errorf("session Job template: %w", err)
	}
	if err := setSecretVolume(pod, name); err != nil {
		return nil, err
	}
	containers, _ := pod["containers"].([]any)
	if len(containers) == 0 {
		return nil, errors.New("the session Job template has no container")
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		return nil, errors.New("the session Job template's container is not an object")
	}
	checkout := "new"
	if s.existing {
		checkout = "existing"
	}
	resume := ""
	if s.resume {
		resume = "1"
	}
	// An organization-scoped session that has not chosen its repository yet gets none: it clones the one it fixes itself.
	repo, fullName := s.project.repoFor(s.task), ""
	if repo.Name != "" {
		fullName = repo.Owner + "/" + repo.Name
	}
	setEnv(container, map[string]string{
		"ARBETERN_GATEWAY_URL":    c.cfg.GatewayURL,
		"ARBETERN_WORK_ORDER":     s.workOrder(),
		"ARBETERN_OWNER":          repo.Owner,
		"ARBETERN_REPO":           fullName,
		"ARBETERN_BASE_BRANCH":    repo.BaseBranch,
		"ARBETERN_BRANCH":         s.branch,
		"ARBETERN_CHECKOUT":       checkout,
		"ARBETERN_CLAUDE_SESSION": s.claudeSession,
		"ARBETERN_RESUME":         resume,
		"ARBETERN_MODEL":          c.model(s.project),
		"ARBETERN_MAX_TURNS":      strconv.Itoa(s.project.Session.maxTurns()),
		// Claude Code's own aliases, including the haiku it uses for background work, follow arbetern's current models.
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":  llm.AnthropicModel(string(llm.ModelHaiku)),
		"ANTHROPIC_DEFAULT_SONNET_MODEL": llm.AnthropicModel(string(llm.ModelSonnet)),
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   llm.AnthropicModel(string(llm.ModelOpus)),
		"ANTHROPIC_SMALL_FAST_MODEL":     llm.AnthropicModel(string(llm.ModelHaiku)),
	})
	return job, nil
}

func setSecretVolume(pod map[string]any, name string) error {
	volumes, _ := pod["volumes"].([]any)
	for _, v := range volumes {
		volume, _ := v.(map[string]any)
		if volume["name"] != workOrderVolume {
			continue
		}
		secret, ok := volume["secret"].(map[string]any)
		if !ok {
			return fmt.Errorf("the session Job template volume %q is not a secret volume", workOrderVolume)
		}
		secret["secretName"] = name
		return nil
	}
	return fmt.Errorf("the session Job template has no %q volume", workOrderVolume)
}

func setEnv(container map[string]any, vars map[string]string) {
	env, _ := container["env"].([]any)
	env = slices.DeleteFunc(env, func(e any) bool {
		entry, _ := e.(map[string]any)
		name, _ := entry["name"].(string)
		_, replaced := vars[name]
		return replaced
	})
	for _, name := range slices.Sorted(maps.Keys(vars)) {
		env = append(env, map[string]any{"name": name, "value": vars[name]})
	}
	container["env"] = env
}

func newSessionCredentials() (id, token string, err error) {
	b := make([]byte, 16+32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	id = localSessionPrefix + hex.EncodeToString(b[:16])
	return id, id + "." + base64.RawURLEncoding.EncodeToString(b[16:]), nil
}

func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func localSessionID(token string) (string, bool) {
	id, _, ok := strings.Cut(token, ".")
	if !ok || !strings.HasPrefix(id, localSessionPrefix) || !sessionKeyRe.MatchString(id) {
		return "", false
	}
	return id, true
}

func tokenMatches(token, hash string) bool {
	want, err := hex.DecodeString(hash)
	if err != nil || len(want) != sha256.Size {
		return false
	}
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], want) == 1
}

// started identifies the session a Job runs.
type started struct {
	SessionID     string
	ClaudeSession string
	Branch        string
}

func taskBranch(p *Project, t *Task) string {
	return BranchPrefix + store.Slugify(p.Name, "project") + "-" + t.ID
}

// launch binds and starts the session of a task or of one of its follow-up rounds.
func (r *Registry) launch(ctx context.Context, p *Project, t *Task, round int) (*started, error) {
	id, token, err := newSessionCredentials()
	if err != nil {
		return nil, err
	}
	// A round resumes the task's conversation; the entrypoint starts a new one when no transcript is stored.
	claudeSession, resume := t.ClaudeSession, round > 0
	if claudeSession == "" {
		if claudeSession, err = newUUID(); err != nil {
			return nil, err
		}
		resume = false
	}
	branch, existing := taskBranch(p, t), false
	if round > 0 {
		branch, existing = t.Branch, true
	}
	// A project deleted on another replica must not get a Job from a tick that still holds its descriptor.
	if _, err := r.fetchProject(ctx, p.ID); err != nil {
		return nil, fmt.Errorf("reading the project before dispatch: %w", err)
	}
	b := binding{Project: p.ID, Task: t.ID, Round: round, TokenHash: tokenHash(token), ClaudeSession: claudeSession, Branch: branch, CreatedAt: stamp(time.Now())}
	if err := r.writeBinding(ctx, id, b); err != nil {
		return nil, fmt.Errorf("storing the session binding: %w", err)
	}
	s := clusterSession{
		id:            id,
		token:         token,
		project:       p,
		task:          t,
		round:         round,
		branch:        branch,
		existing:      existing,
		claudeSession: claudeSession,
		resume:        resume,
		deadline:      time.Duration(p.Limits.effective().SessionTimeoutMinutes)*time.Minute + jobDeadlineGrace,
	}
	if err := r.cluster.start(ctx, s); err != nil {
		// The API server may have kept a Job whose creation answered with an error.
		r.cluster.stopSession(context.WithoutCancel(ctx), id)
		r.deleteBinding(context.WithoutCancel(ctx), id)
		return nil, fmt.Errorf("starting the session Job: %w", err)
	}
	return &started{SessionID: id, ClaudeSession: claudeSession, Branch: branch}, nil
}
