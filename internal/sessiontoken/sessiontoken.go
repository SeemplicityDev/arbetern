// Package sessiontoken verifies the session tokens Claude Code self-hosted runners present.
package sessiontoken

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/justmike1/arbetern/internal/httpx"
	"github.com/justmike1/arbetern/internal/ttlcache"
)

// JWKSURL publishes the keys that sign self-hosted session tokens.
const JWKSURL = "https://api.anthropic.com/v1/code/.well-known/jwks.json"

const (
	tokenPrefix    = "sk-ant-cc-"
	maxTokenBytes  = 8 << 10
	clockLeeway    = 60 * time.Second
	issuer         = "ccr"
	sessionRole    = "session_worker"
	maxUnixSeconds = 1 << 40
)

// ErrInvalid is wrapped by every verification failure.
var ErrInvalid = errors.New("invalid session token")

// ErrUnavailable is returned while no signing keys could be loaded, so no token can be judged either way.
var ErrUnavailable = errors.New("session token signing keys are unavailable")

var (
	segmentEncoding = base64.RawURLEncoding.Strict()
	sessionIDRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// Claims is the verified identity a session token carries.
type Claims struct {
	SessionID string    // ccr:session_id, normalized with NormalizeSessionID
	PoolID    string    // ccr:pool_id
	OrgID     string    // ccr:org_id
	AccountID string    // ccr:account_id; empty for agent sessions
	Actor     string    // act.sub, "user:<id>" or "agent:<id>"
	TokenID   string    // jti
	Expires   time.Time // exp
}

// Verifier checks session tokens issued for one self-hosted environment.
type Verifier struct {
	audience string
	client   *http.Client
	jwksURL  string
	keys     *ttlcache.Cache[keySet]
	lastGood atomic.Pointer[keySet]

	fetchMu   sync.Mutex
	lastFetch time.Time
}

// NewVerifier returns a Verifier requiring audience (the ccpool_ environment id); a nil client uses httpx.SafeClient.
func NewVerifier(audience string, client *http.Client) *Verifier {
	if client == nil {
		client = httpx.SafeClient(10*time.Second, 0)
	}
	v := &Verifier{audience: strings.TrimSpace(audience), client: client, jwksURL: JWKSURL}
	v.keys = ttlcache.New(keysTTL, v.fetchKeys)
	return v
}

// Verify checks token, the Authorization value after "Bearer ", and returns its claims.
func (v *Verifier) Verify(ctx context.Context, token string) (*Claims, error) {
	if v.audience == "" {
		return nil, invalid("no environment id configured")
	}
	if len(token) > maxTokenBytes {
		return nil, invalid("token is longer than %d bytes", maxTokenBytes)
	}
	if rest, ok := strings.CutPrefix(token, tokenPrefix); ok {
		token = rest
	} else if strings.HasPrefix(token, "sk-ant-") {
		return nil, invalid("not a self-hosted session token")
	}
	parts := strings.SplitN(token, ".", 4)
	if len(parts) != 3 {
		return nil, invalid("token does not have three segments")
	}
	var segs [3][]byte
	for i, p := range parts {
		b, err := decodeSegment(p)
		if err != nil {
			return nil, invalid("segment %d is not base64url", i+1)
		}
		segs[i] = b
	}
	kid, err := parseHeader(segs[0])
	if err != nil {
		return nil, err
	}
	if len(segs[2]) != 64 {
		return nil, invalid("signature is %d bytes, want 64", len(segs[2]))
	}
	pub, err := v.key(ctx, kid)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(segs[2][:32]), new(big.Int).SetBytes(segs[2][32:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return nil, invalid("signature does not verify")
	}
	return v.claims(segs[1], time.Now())
}

// NormalizeSessionID strips a leading "session_" or "cse_" and returns "" unless the rest matches ^[A-Za-z0-9_-]{1,128}$.
func NormalizeSessionID(id string) string {
	if rest, ok := strings.CutPrefix(id, "session_"); ok {
		id = rest
	} else {
		id = strings.TrimPrefix(id, "cse_")
	}
	if !sessionIDRe.MatchString(id) {
		return ""
	}
	return id
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func decodeSegment(s string) ([]byte, error) {
	// The decoder skips CR and LF even in strict mode.
	if strings.ContainsAny(s, "\r\n") {
		return nil, errors.New("line break in segment")
	}
	return segmentEncoding.DecodeString(s)
}

func parseHeader(b []byte) (string, error) {
	var h map[string]json.RawMessage
	if err := json.Unmarshal(b, &h); err != nil {
		return "", invalid("malformed header")
	}
	if _, ok := h["crit"]; ok {
		return "", invalid("critical header parameters are not supported")
	}
	var alg, kid string
	if json.Unmarshal(h["alg"], &alg) != nil || alg != "ES256" {
		return "", invalid("algorithm is not ES256")
	}
	if json.Unmarshal(h["kid"], &kid) != nil || kid == "" {
		return "", invalid("missing key id")
	}
	return kid, nil
}

type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

type rawClaims struct {
	Iss       string   `json:"iss"`
	Aud       audience `json:"aud"`
	Exp       *float64 `json:"exp"`
	Iat       *float64 `json:"iat"`
	Nbf       *float64 `json:"nbf"`
	Jti       string   `json:"jti"`
	Role      string   `json:"ccr:role"`
	SessionID string   `json:"ccr:session_id"`
	PoolID    string   `json:"ccr:pool_id"`
	OrgID     string   `json:"ccr:org_id"`
	AccountID string   `json:"ccr:account_id"`
	Act       struct {
		Sub string `json:"sub"`
	} `json:"act"`
}

func (v *Verifier) claims(payload []byte, now time.Time) (*Claims, error) {
	var c rawClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, invalid("malformed claims")
	}
	nowSec := float64(now.UnixNano()) / 1e9
	leeway := clockLeeway.Seconds()
	switch {
	case c.Iss != issuer:
		return nil, invalid("issuer is not %s", issuer)
	case !slices.Contains(c.Aud, v.audience):
		return nil, invalid("audience does not include the environment")
	case c.Role != sessionRole:
		return nil, invalid("role is not %s", sessionRole)
	case c.PoolID != v.audience:
		return nil, invalid("pool id does not match the environment")
	case c.Exp == nil:
		return nil, invalid("missing exp")
	case *c.Exp+leeway <= nowSec:
		return nil, invalid("token expired at %s", unixTime(*c.Exp).Format(time.RFC3339))
	case c.Iat != nil && *c.Iat > nowSec+leeway:
		return nil, invalid("token issued in the future")
	case c.Nbf != nil && *c.Nbf > nowSec+leeway:
		return nil, invalid("token not valid before %s", unixTime(*c.Nbf).Format(time.RFC3339))
	}
	sid := NormalizeSessionID(c.SessionID)
	if sid == "" {
		return nil, invalid("missing or malformed ccr:session_id")
	}
	return &Claims{
		SessionID: sid,
		PoolID:    c.PoolID,
		OrgID:     c.OrgID,
		AccountID: c.AccountID,
		Actor:     c.Act.Sub,
		TokenID:   c.Jti,
		Expires:   unixTime(*c.Exp),
	}, nil
}

func unixTime(sec float64) time.Time {
	return time.Unix(int64(min(max(sec, 0), maxUnixSeconds)), 0).UTC()
}
