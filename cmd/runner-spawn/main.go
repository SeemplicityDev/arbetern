// Command runner-spawn is the orchestrator's spawn-runner hook: it submits one Kubernetes Job and work-order Secret per session.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	exitSubmitted = 0
	exitRetryable = 1
	exitPermanent = 2

	runTimeout        = 45 * time.Second
	serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("runner-spawn: ")
	err := run()
	if err != nil {
		log.Print(err)
	}
	os.Exit(exitCode(err))
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
	defer stop()

	cfg := config{
		WorkOrderFile:  env("CLAUDE_RUNNER_WORK_ORDER_FILE"),
		OrderID:        env("CLAUDE_RUNNER_ORDER_ID"),
		SessionID:      env("CLAUDE_RUNNER_SESSION_ID"),
		SessionUUID:    env("CLAUDE_RUNNER_SESSION_UUID"),
		AccountID:      env("CLAUDE_RUNNER_ACCOUNT_ID"),
		AllowedAccount: env("RUNNER_ACCOUNT_ID"),
		JobTemplate:    env("RUNNER_JOB_TEMPLATE"),
		Namespace:      env("RUNNER_NAMESPACE"),
	}
	host, port := env("KUBERNETES_SERVICE_HOST"), env("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return permanent(errors.New("KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are not set"))
	}
	token, client, err := inClusterClient(serviceAccountDir)
	if err != nil {
		return permanent(err)
	}
	cfg.Token = token
	return spawn(ctx, cfg, client, "https://"+net.JoinHostPort(host, port))
}

func env(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

func inClusterClient(dir string) (string, *http.Client, error) {
	token, err := os.ReadFile(filepath.Join(dir, "token"))
	if err != nil {
		return "", nil, fmt.Errorf("read service account token: %w", err)
	}
	token = bytes.TrimSpace(token)
	if len(token) == 0 {
		return "", nil, errors.New("service account token is empty")
	}
	ca, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return "", nil, fmt.Errorf("read service account CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return "", nil, errors.New("service account ca.crt holds no PEM certificate")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// HTTPS_PROXY on the orchestrator is meant for Anthropic traffic; the API server is always reached directly.
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return string(token), &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func retryable(err error) error { return &exitError{code: exitRetryable, err: err} }
func permanent(err error) error { return &exitError{code: exitPermanent, err: err} }

func exitCode(err error) int {
	var e *exitError
	switch {
	case err == nil:
		return exitSubmitted
	case errors.As(err, &e):
		return e.code
	default:
		return exitRetryable
	}
}
