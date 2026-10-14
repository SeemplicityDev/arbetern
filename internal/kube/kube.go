// Package kube is a minimal in-cluster Kubernetes API client for the session Jobs.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ServiceAccountDir is where Kubernetes mounts a pod's service account credentials.
const ServiceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

const (
	maxResponse = 1 << 20
	maxMessage  = 1000
)

// Client calls the API server with the pod's service account.
type Client struct {
	http      *http.Client
	base      string
	tokenFile string
	userAgent string
}

// InCluster builds a Client from the service account files in dir and the KUBERNETES_SERVICE_* variables.
func InCluster(dir, userAgent string) (*Client, error) {
	host, port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST")), strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	if host == "" || port == "" {
		return nil, errors.New("KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are not set")
	}
	ca, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("read service account CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("service account ca.crt holds no PEM certificate")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// A proxy configured for outbound traffic never applies to the API server.
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	c := &Client{
		http: &http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		base:      "https://" + net.JoinHostPort(host, port),
		tokenFile: filepath.Join(dir, "token"),
		userAgent: userAgent,
	}
	if _, err := c.token(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) token() (string, error) {
	// Read on every call: projected service account tokens rotate.
	raw, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return "", fmt.Errorf("read service account token: %w", err)
	}
	t := strings.TrimSpace(string(raw))
	if t == "" {
		return "", errors.New("service account token is empty")
	}
	return t, nil
}

// Do sends body as JSON and returns the status code and up to 1 MiB of the response.
func (c *Client) Do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		payload = bytes.NewReader(b)
	}
	token, err := c.token()
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, payload)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
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

// StatusError describes a failed call on one bounded line, with every value in redact replaced.
func StatusError(op string, code int, body []byte, redact ...string) error {
	msg := fmt.Sprintf("%s: %d %s", op, code, http.StatusText(code))
	var status struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &status) == nil && status.Message != "" {
		msg += ": " + Clean(status.Message, redact...)
	}
	return errors.New(msg)
}

// Clean keeps a server message to one bounded line, with every value in redact replaced; the API server may echo request bodies back.
func Clean(s string, redact ...string) string {
	for _, v := range redact {
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

// UID reads metadata.uid from an object the API server returned.
func UID(body []byte) (string, bool) {
	var obj struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
	}
	if json.Unmarshal(body, &obj) != nil || obj.Metadata.UID == "" {
		return "", false
	}
	return obj.Metadata.UID, true
}

// Walk returns the object at a dotted path, creating missing objects on the way.
func Walk(obj map[string]any, path string) (map[string]any, error) {
	for key := range strings.SplitSeq(path, ".") {
		switch v := obj[key].(type) {
		case map[string]any:
			obj = v
		case nil:
			child := map[string]any{}
			obj[key] = child
			obj = child
		default:
			return nil, fmt.Errorf("field %s is not an object", path)
		}
	}
	return obj, nil
}
