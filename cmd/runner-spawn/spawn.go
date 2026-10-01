package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	namePrefix        = "claude-runner-"
	maxNameLen        = 63
	orderLabel        = "arbetern.io/order"
	sessionLabel      = "arbetern.io/session-hash"
	sessionAnnotation = "arbetern.io/session"
	workOrderVolume   = "work-order"

	maxWorkOrder   = 16 << 10
	maxResponse    = 1 << 20
	maxMessage     = 1000
	cleanupTimeout = 5 * time.Second
	userAgent      = "arbetern/runner-spawn"
)

type config struct {
	WorkOrderFile  string
	OrderID        string
	SessionID      string
	SessionUUID    string
	AccountID      string
	AllowedAccount string
	JobTemplate    string
	Namespace      string
	Token          string
}

func spawn(ctx context.Context, cfg config, client *http.Client, apiBase string) error {
	if cfg.SessionID == "" {
		return permanent(errors.New("pre-warm requests are not supported"))
	}
	if cfg.AllowedAccount != "" && cfg.AccountID != cfg.AllowedAccount {
		return permanent(errors.New("session was not dispatched by the project automation account"))
	}
	order, err := readWorkOrder(cfg.WorkOrderFile)
	if err != nil {
		return retryable(err)
	}
	if cfg.OrderID == "" {
		return permanent(errors.New("CLAUDE_RUNNER_ORDER_ID is not set"))
	}
	if cfg.Namespace == "" {
		return permanent(errors.New("RUNNER_NAMESPACE is not set"))
	}
	name := jobName(cfg.OrderID)
	session := cmp.Or(cfg.SessionUUID, cfg.SessionID)
	sessionHash := sha256Hex(session)[:20]
	job, labels, err := renderJob(cfg.JobTemplate, cfg.Namespace, name, sha256Hex(cfg.OrderID)[:20], sessionHash, session)
	if err != nil {
		return permanent(err)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(order))
	api := &kubeAPI{
		client: client,
		base:   strings.TrimRight(apiBase, "/"),
		token:  cfg.Token,
		ns:     cfg.Namespace,
		redact: []string{order, encoded},
	}
	uid, err := api.createJob(ctx, name, job)
	if err != nil {
		return err
	}
	if err := api.createSecret(ctx, name, uid, labels, encoded); err != nil {
		api.deleteJob(ctx, name, uid)
		return retryable(err)
	}
	log.Printf("submitted job %s for session %s", name, cfg.SessionID)
	// A new order for the session supersedes its earlier ones, whose Jobs that never started only hold resources.
	api.deleteUnstarted(ctx, sessionHash, name)
	return nil
}

func readWorkOrder(path string) (string, error) {
	if path == "" {
		return "", errors.New("CLAUDE_RUNNER_WORK_ORDER_FILE is not set")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("work order: %w", err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxWorkOrder+1))
	if err != nil {
		return "", fmt.Errorf("work order: %w", err)
	}
	if len(raw) > maxWorkOrder {
		return "", fmt.Errorf("work order is larger than %d bytes", maxWorkOrder)
	}
	order := strings.TrimSpace(string(raw))
	if order == "" {
		return "", errors.New("work order is empty")
	}
	return order, nil
}

func jobName(orderID string) string {
	id := strings.Trim(strings.Map(func(r rune) rune {
		if 'a' <= r && r <= 'z' || '0' <= r && r <= '9' {
			return r
		}
		return '-'
	}, strings.ToLower(orderID)), "-")
	name := namePrefix + id
	// Any lossy mapping also gets the hash suffix, so distinct order ids never share a Job.
	if id != "" && id == orderID && len(name) <= maxNameLen {
		return name
	}
	keep := strings.TrimRight(name[:min(len(name), maxNameLen-11)], "-")
	return keep + "-" + sha256Hex(orderID)[:10]
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func renderJob(path, namespace, name, orderHash, sessionHash, session string) (map[string]any, map[string]any, error) {
	if path == "" {
		return nil, nil, errors.New("RUNNER_JOB_TEMPLATE is not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read job template: %w", err)
	}
	var job map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&job); err != nil {
		return nil, nil, fmt.Errorf("parse job template %s: %w", path, err)
	}
	if job == nil {
		return nil, nil, fmt.Errorf("job template %s is empty", path)
	}
	labels, err := walk(job, "metadata.labels")
	if err != nil {
		return nil, nil, err
	}
	labels[orderLabel] = orderHash
	labels[sessionLabel] = sessionHash
	for _, set := range []struct{ path, key, value string }{
		{"metadata", "name", name},
		{"metadata", "namespace", namespace},
		{"metadata.annotations", sessionAnnotation, session},
		{"spec.template.metadata.labels", orderLabel, orderHash},
		{"spec.template.metadata.labels", sessionLabel, sessionHash},
		{"spec.template.metadata.annotations", sessionAnnotation, session},
	} {
		obj, err := walk(job, set.path)
		if err != nil {
			return nil, nil, err
		}
		obj[set.key] = set.value
	}
	podSpec, err := walk(job, "spec.template.spec")
	if err != nil {
		return nil, nil, err
	}
	volumes, _ := podSpec["volumes"].([]any)
	for _, v := range volumes {
		volume, _ := v.(map[string]any)
		if volume["name"] != workOrderVolume {
			continue
		}
		secret, ok := volume["secret"].(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("job template volume %q is not a secret volume", workOrderVolume)
		}
		secret["secretName"] = name
		return job, labels, nil
	}
	return nil, nil, fmt.Errorf("job template has no %q volume", workOrderVolume)
}

func walk(obj map[string]any, path string) (map[string]any, error) {
	for key := range strings.SplitSeq(path, ".") {
		switch v := obj[key].(type) {
		case map[string]any:
			obj = v
		case nil:
			child := map[string]any{}
			obj[key] = child
			obj = child
		default:
			return nil, fmt.Errorf("job template field %s is not an object", path)
		}
	}
	return obj, nil
}

type kubeAPI struct {
	client *http.Client
	base   string
	token  string
	ns     string
	redact []string
}

func (k *kubeAPI) jobs() string {
	return "/apis/batch/v1/namespaces/" + url.PathEscape(k.ns) + "/jobs"
}

func (k *kubeAPI) createJob(ctx context.Context, name string, job map[string]any) (string, error) {
	op := "create job " + name
	code, body, err := k.do(ctx, http.MethodPost, k.jobs(), job)
	switch {
	case err != nil:
		return "", retryable(fmt.Errorf("%s: %w", op, err))
	case code == http.StatusCreated || code == http.StatusOK:
		return uidOf(op, body)
	case code == http.StatusConflict:
		log.Printf("job %s already exists; reusing it", name)
		return k.jobUID(ctx, name)
	default:
		return "", classify(code, k.statusError(op, code, body))
	}
}

func (k *kubeAPI) jobUID(ctx context.Context, name string) (string, error) {
	op := "get job " + name
	code, body, err := k.do(ctx, http.MethodGet, k.jobs()+"/"+name, nil)
	switch {
	case err != nil:
		return "", retryable(fmt.Errorf("%s: %w", op, err))
	case code == http.StatusOK:
		return uidOf(op, body)
	case code == http.StatusNotFound:
		// Deleted after the create conflict; the next attempt can create it again.
		return "", retryable(k.statusError(op, code, body))
	default:
		return "", classify(code, k.statusError(op, code, body))
	}
}

func (k *kubeAPI) createSecret(ctx context.Context, name, uid string, labels map[string]any, encoded string) error {
	secret := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":   name,
			"labels": labels,
			"ownerReferences": []map[string]string{{
				"apiVersion": "batch/v1",
				"kind":       "Job",
				"name":       name,
				"uid":        uid,
			}},
		},
		"type":      "Opaque",
		"immutable": true,
		"data":      map[string]string{"token": encoded},
	}
	op := "create secret " + name
	code, body, err := k.do(ctx, http.MethodPost, "/api/v1/namespaces/"+url.PathEscape(k.ns)+"/secrets", secret)
	switch {
	case err != nil:
		return fmt.Errorf("%s: %w", op, err)
	case code == http.StatusCreated || code == http.StatusOK || code == http.StatusConflict:
		return nil
	default:
		return k.statusError(op, code, body)
	}
}

func (k *kubeAPI) deleteJob(ctx context.Context, name, uid string) {
	// Detached so a run that timed out or was signalled still removes the Job it could not finish.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	opts := map[string]any{
		"apiVersion":        "v1",
		"kind":              "DeleteOptions",
		"propagationPolicy": "Background",
		"preconditions":     map[string]string{"uid": uid},
	}
	code, body, err := k.do(ctx, http.MethodDelete, k.jobs()+"/"+name, opts)
	switch {
	case err != nil:
		log.Printf("delete job %s: %v", name, err)
	case code/100 == 2 || code == http.StatusNotFound:
		log.Printf("deleted job %s", name)
	default:
		log.Print(k.statusError("delete job "+name, code, body))
	}
}

func (k *kubeAPI) deleteUnstarted(ctx context.Context, sessionHash, keep string) {
	op := "list jobs"
	code, body, err := k.do(ctx, http.MethodGet, k.jobs()+"?labelSelector="+url.QueryEscape(sessionLabel+"="+sessionHash), nil)
	switch {
	case err != nil:
		log.Printf("%s: %v", op, err)
		return
	case code != http.StatusOK:
		log.Print(k.statusError(op, code, body))
		return
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
				UID  string `json:"uid"`
			} `json:"metadata"`
			Status struct {
				Ready     *int `json:"ready"`
				Succeeded int  `json:"succeeded"`
				Failed    int  `json:"failed"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		log.Printf("%s: %v", op, err)
		return
	}
	for _, job := range list.Items {
		st := job.Status
		if job.Metadata.Name == keep || st.Ready == nil || *st.Ready > 0 || st.Succeeded > 0 || st.Failed > 0 {
			continue
		}
		log.Printf("job %s of this session never started; deleting it", job.Metadata.Name)
		k.deleteJob(ctx, job.Metadata.Name, job.Metadata.UID)
	}
}

func (k *kubeAPI) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, payload)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+k.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return 0, nil, fmt.Errorf("read response: %w", err)
	}
	return resp.StatusCode, data, nil
}

func (k *kubeAPI) statusError(op string, code int, body []byte) error {
	msg := fmt.Sprintf("%s: %d %s", op, code, http.StatusText(code))
	var status struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &status) == nil && status.Message != "" {
		msg += ": " + k.clean(status.Message)
	}
	return errors.New(msg)
}

// clean keeps a server message to one bounded line and scrubs the work order, which the API server may echo back.
func (k *kubeAPI) clean(s string) string {
	for _, v := range k.redact {
		if v != "" {
			s = strings.ReplaceAll(s, v, "<redacted>")
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxMessage {
		s = strings.ToValidUTF8(s[:maxMessage], "") + "..."
	}
	return s
}

func uidOf(op string, body []byte) (string, error) {
	var obj struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
	}
	if json.Unmarshal(body, &obj) != nil || obj.Metadata.UID == "" {
		return "", retryable(fmt.Errorf("%s: response has no metadata.uid", op))
	}
	return obj.Metadata.UID, nil
}

func classify(code int, err error) error {
	if code == http.StatusUnauthorized || code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500 {
		return retryable(err)
	}
	return permanent(err)
}
