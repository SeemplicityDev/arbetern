package sessiontoken

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

const (
	keysTTL         = 5 * time.Minute
	refetchInterval = 30 * time.Second
	fetchTimeout    = 10 * time.Second
	maxJWKSBytes    = 64 << 10
	userAgent       = "arbetern/sessiontoken"
)

type keySet map[string]*ecdsa.PublicKey

type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (v *Verifier) key(ctx context.Context, kid string) (*ecdsa.PublicKey, error) {
	ks, err := v.keySet(ctx)
	if err != nil {
		return nil, err
	}
	if k, ok := ks[kid]; ok {
		return k, nil
	}
	if v.claimRefetch() {
		v.keys.Invalidate()
		if ks, err = v.keySet(ctx); err != nil {
			return nil, err
		}
		if k, ok := ks[kid]; ok {
			return k, nil
		}
	}
	return nil, invalid("unknown key id")
}

// keySet falls back to lastGood because Invalidate drops the cached set even when the refetch that follows fails.
func (v *Verifier) keySet(ctx context.Context) (keySet, error) {
	ks, err := v.keys.Get(ctx)
	if err == nil {
		return ks, nil
	}
	if last := v.lastGood.Load(); last != nil {
		return *last, nil
	}
	return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
}

// claimRefetch allows an unknown-kid refetch only when no fetch started in the last refetchInterval.
func (v *Verifier) claimRefetch() bool {
	v.fetchMu.Lock()
	defer v.fetchMu.Unlock()
	if time.Since(v.lastFetch) < refetchInterval {
		return false
	}
	v.lastFetch = time.Now()
	return true
}

func (v *Verifier) fetchKeys(ctx context.Context) (keySet, error) {
	v.fetchMu.Lock()
	v.lastFetch = time.Now()
	v.fetchMu.Unlock()
	ks, err := v.download(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("[sessiontoken] refreshing signing keys: %v", err)
		}
		return nil, err
	}
	v.lastGood.Store(&ks)
	return ks, nil
}

func (v *Verifier) download(ctx context.Context) (keySet, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building JWKS request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching JWKS: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching JWKS: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading JWKS: %w", err)
	}
	if len(body) > maxJWKSBytes {
		return nil, fmt.Errorf("JWKS is larger than %d bytes", maxJWKSBytes)
	}
	return parseJWKS(body)
}

func parseJWKS(body []byte) (keySet, error) {
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decoding JWKS: %w", err)
	}
	ks := keySet{}
	for _, raw := range doc.Keys {
		var k jwk
		if json.Unmarshal(raw, &k) != nil || k.Kid == "" || ks[k.Kid] != nil {
			continue
		}
		if pub := k.publicKey(); pub != nil {
			ks[k.Kid] = pub
		}
	}
	if len(ks) == 0 {
		return nil, errors.New("JWKS has no usable ES256 keys")
	}
	return ks, nil
}

func (k jwk) publicKey() *ecdsa.PublicKey {
	if k.Kty != "EC" || k.Crv != "P-256" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "ES256") {
		return nil
	}
	x, errX := base64.RawURLEncoding.DecodeString(k.X)
	y, errY := base64.RawURLEncoding.DecodeString(k.Y)
	if errX != nil || errY != nil || len(x) != 32 || len(y) != 32 {
		return nil
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
	if err != nil {
		return nil
	}
	return pub
}
