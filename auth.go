package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ── OAuth token cache — maps user access tokens to verified Slack user IDs ──

type tokenEntry struct {
	userID  string
	fetched time.Time
}

type tokenCache struct {
	mu      sync.RWMutex
	entries map[string]*tokenEntry
	ttl     time.Duration
}

func newTokenCache(ttl time.Duration) *tokenCache {
	return &tokenCache{
		entries: make(map[string]*tokenEntry),
		ttl:     ttl,
	}
}

// resolve returns the verified Slack user ID for the given access token.
// It checks the cache first and falls back to calling Slack's auth.test API.
func (tc *tokenCache) resolve(accessToken string) (string, error) {
	tc.mu.RLock()
	entry, ok := tc.entries[accessToken]
	if ok && time.Since(entry.fetched) < tc.ttl {
		tc.mu.RUnlock()
		return entry.userID, nil
	}
	tc.mu.RUnlock()

	userID, err := verifySlackToken(accessToken)
	if err != nil {
		return "", err
	}

	tc.mu.Lock()
	tc.entries[accessToken] = &tokenEntry{userID: userID, fetched: time.Now()}
	tc.mu.Unlock()

	log.Printf("[auth] verified token for user %s", userID)
	return userID, nil
}

// verifySlackToken calls Slack's auth.test with the given user token and
// returns the authenticated user ID.
func verifySlackToken(token string) (string, error) {
	data := url.Values{"token": {token}}
	resp, err := http.PostForm("https://slack.com/api/auth.test", data)
	if err != nil {
		return "", fmt.Errorf("slack auth.test request failed: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	var result struct {
		OK     bool   `json:"ok"`
		UserID string `json:"user_id"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode auth.test response: %w", err)
	}
	if !result.OK {
		return "", fmt.Errorf("slack auth.test failed: %s", result.Error)
	}
	return result.UserID, nil
}

// extractBearerToken returns the token from an "Authorization: Bearer <token>"
// header, or an empty string if the header is missing or malformed.
func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return ""
	}
	return strings.TrimSpace(auth[len(prefix):])
}

// ── OAuth flow — Slack "Sign in with Slack" for the direct API ──────────────

type oauthStateStore struct {
	mu      sync.Mutex
	pending map[string]time.Time // state -> creation time
	ttl     time.Duration
}

func newOAuthStateStore(ttl time.Duration) *oauthStateStore {
	return &oauthStateStore{
		pending: make(map[string]time.Time),
		ttl:     ttl,
	}
}

func (s *oauthStateStore) generate() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	state := hex.EncodeToString(b)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Purge expired states while we hold the lock.
	now := time.Now()
	for k, created := range s.pending {
		if now.Sub(created) > s.ttl {
			delete(s.pending, k)
		}
	}
	s.pending[state] = now
	return state, nil
}

func (s *oauthStateStore) validate(state string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	created, ok := s.pending[state]
	if !ok {
		return false
	}
	delete(s.pending, state)
	return time.Since(created) <= s.ttl
}

// handleOAuthStart returns an HTTP handler that redirects the user to
// Slack's OAuth authorize page.
func handleOAuthStart(clientID, appURL string, states *oauthStateStore) http.HandlerFunc {
	redirectURI := strings.TrimRight(appURL, "/") + "/api/auth/callback"

	return func(w http.ResponseWriter, r *http.Request) {
		state, err := states.generate()
		if err != nil {
			http.Error(w, "failed to generate state", http.StatusInternalServerError)
			return
		}

		params := url.Values{
			"client_id":    {clientID},
			"user_scope":   {"identity.basic"},
			"redirect_uri": {redirectURI},
			"state":        {state},
		}
		target := "https://slack.com/oauth/v2/authorize?" + params.Encode()
		http.Redirect(w, r, target, http.StatusFound)
	}
}

// handleOAuthCallback returns an HTTP handler that exchanges the Slack
// authorization code for a user access token and returns it to the caller.
func handleOAuthCallback(clientID, clientSecret, appURL string, states *oauthStateStore) http.HandlerFunc {
	redirectURI := strings.TrimRight(appURL, "/") + "/api/auth/callback"

	return func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		state := r.URL.Query().Get("state")
		if code == "" || state == "" {
			http.Error(w, "missing code or state", http.StatusBadRequest)
			return
		}
		if !states.validate(state) {
			http.Error(w, "invalid or expired state", http.StatusBadRequest)
			return
		}

		data := url.Values{
			"client_id":     {clientID},
			"client_secret": {clientSecret},
			"code":          {code},
			"redirect_uri":  {redirectURI},
		}
		resp, err := http.PostForm("https://slack.com/api/oauth.v2.access", data)
		if err != nil {
			log.Printf("[auth] oauth.v2.access request failed: %v", err)
			http.Error(w, "failed to exchange code", http.StatusBadGateway)
			return
		}
		defer func() {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()

		var result struct {
			OK         bool   `json:"ok"`
			Error      string `json:"error"`
			AuthedUser struct {
				ID          string `json:"id"`
				AccessToken string `json:"access_token"`
			} `json:"authed_user"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			log.Printf("[auth] failed to decode oauth.v2.access response: %v", err)
			http.Error(w, "failed to decode response", http.StatusBadGateway)
			return
		}
		if !result.OK {
			log.Printf("[auth] oauth.v2.access error: %s", result.Error)
			http.Error(w, fmt.Sprintf("Slack OAuth error: %s", result.Error), http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token": result.AuthedUser.AccessToken,
			"user_id":      result.AuthedUser.ID,
		})
	}
}
