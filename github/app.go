package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v91/github"
)

// AppConfig names the GitHub App installation a Client acts as.
type AppConfig struct {
	ID             string // App ID or client ID
	InstallationID int64  // 0 uses the App's only installation
	PrivateKey     string // PEM, raw or base64-encoded
}

const (
	appJWTLifetime = 5 * time.Minute
	// Installation tokens last an hour; one this close to its expiry is replaced before use.
	appTokenRenewal = 10 * time.Minute
	appCallTimeout  = 20 * time.Second
	appRetryDelay   = 15 * time.Second
)

var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))

type appAuth struct {
	id  string
	key *rsa.PrivateKey
	api *gh.Client // authenticates as the App itself, for the /app endpoints

	mu        sync.Mutex
	installID int64
	account   string
	slug      string
	token     string
	expires   time.Time
	perms     map[string]string
	retryAt   time.Time
	err       error
}

// bearer authorizes each request with the credential it returns when the request is sent.
type bearer func(ctx context.Context) (string, error)

func (auth bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	cred, err := auth(req.Context())
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+cred)
	return http.DefaultTransport.RoundTrip(req)
}

func newAppAuth(cfg AppConfig) (*appAuth, error) {
	id := strings.TrimSpace(cfg.ID)
	if id == "" {
		return nil, errors.New("the GitHub App ID is empty")
	}
	key, err := parseAppKey(cfg.PrivateKey)
	if err != nil {
		return nil, err
	}
	a := &appAuth{id: id, key: key, installID: cfg.InstallationID}
	sign := bearer(func(context.Context) (string, error) { return a.jwt(time.Now()) })
	if a.api, err = gh.NewClient(gh.WithTransport(sign), gh.WithTimeout(apiTimeout)); err != nil {
		return nil, err
	}
	return a, nil
}

func parseAppKey(s string) (*rsa.PrivateKey, error) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "-----BEGIN") {
		if raw, err := base64.StdEncoding.DecodeString(s); err == nil {
			s = string(raw)
		}
	}
	block, _ := pem.Decode([]byte(strings.ReplaceAll(s, `\n`, "\n")))
	if block == nil {
		return nil, errors.New("the GitHub App private key is not a PEM block")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the GitHub App private key cannot be parsed: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the GitHub App private key is not an RSA key")
	}
	return key, nil
}

func (a *appAuth) jwt(now time.Time) (string, error) {
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(), // GitHub's margin for clock drift
		"exp": now.Add(appJWTLifetime).Unix(),
		"iss": a.id,
	})
	if err != nil {
		return "", err
	}
	signed := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Token returns the installation's access token, minting a new one when the current one nears its expiry.
func (a *appAuth) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	valid := a.token != "" && now.Before(a.expires.Add(-time.Minute))
	if valid && now.Before(a.expires.Add(-appTokenRenewal)) {
		return a.token, nil
	}
	if now.Before(a.retryAt) {
		if valid {
			return a.token, nil
		}
		return "", a.err
	}
	// Every caller waits on this mint, so one caller's cancellation must not fail it for the others.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), appCallTimeout)
	defer cancel()
	err := a.mint(ctx)
	if err == nil {
		return a.token, nil
	}
	a.retryAt, a.err = now.Add(appRetryDelay), err
	if !valid {
		return "", err
	}
	log.Printf("[github] renewing the GitHub App token failed, the current one stays in use: %v", err)
	return a.token, nil
}

func (a *appAuth) mint(ctx context.Context) error {
	id, err := a.installation(ctx)
	if err != nil {
		return err
	}
	tok, _, err := a.api.Apps.CreateInstallationToken(ctx, id, nil)
	if err != nil {
		return fmt.Errorf("creating a GitHub App installation token: %w", err)
	}
	perms := map[string]string{}
	if raw, err := json.Marshal(tok.GetPermissions()); err == nil {
		_ = json.Unmarshal(raw, &perms)
	}
	a.token, a.expires, a.perms, a.err = tok.GetToken(), tok.GetExpiresAt().Time, perms, nil
	return nil
}

// installation returns the installation ID, adopting the App's only installation when none is configured; a.mu must be held.
func (a *appAuth) installation(ctx context.Context) (int64, error) {
	if a.installID != 0 {
		return a.installID, nil
	}
	list, _, err := a.api.Apps.ListInstallations(ctx, &gh.ListOptions{PerPage: 2})
	if err != nil {
		return 0, fmt.Errorf("listing the GitHub App's installations: %w", err)
	}
	switch len(list) {
	case 0:
		return 0, errors.New("the GitHub App is not installed on any account")
	case 1:
		a.installID, a.account = list[0].GetID(), list[0].GetAccount().GetLogin()
		return a.installID, nil
	}
	return 0, errors.New("the GitHub App is installed on more than one account, so its installation ID (GITHUB_APP_INSTALLATION_ID) must be set")
}

// owner returns the account the App is installed on.
func (a *appAuth) owner(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.account != "" {
		return a.account, nil
	}
	id, err := a.installation(ctx)
	if err != nil {
		return "", err
	}
	if a.account == "" {
		inst, _, err := a.api.Apps.GetInstallation(ctx, id)
		if err != nil {
			return "", fmt.Errorf("reading the GitHub App installation: %w", err)
		}
		a.account = inst.GetAccount().GetLogin()
	}
	return a.account, nil
}

// login returns the App's bot account as REST names it, "<slug>[bot]"; GraphQL names it by the slug alone.
func (a *appAuth) login(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.slug == "" {
		app, _, err := a.api.Apps.Get(ctx, "")
		if err != nil {
			return "", fmt.Errorf("reading the GitHub App: %w", err)
		}
		a.slug = app.GetSlug()
	}
	return a.slug + "[bot]", nil
}

func (a *appAuth) permissions() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.perms)
}
