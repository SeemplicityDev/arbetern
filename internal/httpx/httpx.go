// Package httpx holds HTTP helpers shared by the API handlers and by every
// outbound client that may be pointed at a user- or model-supplied URL.
package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// CheckSameOrigin rejects state-changing requests that a third-party site
// caused the browser to send. Safe methods always pass. Browsers that send
// Sec-Fetch-Site are judged on it; otherwise an Origin header, when present,
// must match the request host. A client that sends neither is not a browser
// and cannot be driven cross-site, so it passes.
func CheckSameOrigin(r *http.Request) error {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return nil
	case "":
		// Fall through to the Origin check below.
	default:
		return fmt.Errorf("cross-site request rejected")
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" || origin == "null" {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return fmt.Errorf("cross-site request rejected")
	}
	if !strings.EqualFold(u.Host, r.Host) {
		return fmt.Errorf("cross-site request rejected")
	}
	return nil
}

// cgnat and benchmark ranges that net.IP.IsPrivate does not cover.
var extraBlocked = []string{
	"100.64.0.0/10",   // carrier-grade NAT
	"192.0.0.0/24",    // IETF protocol assignments
	"198.18.0.0/15",   // benchmarking
	"192.0.2.0/24",    // TEST-NET-1
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
}

var extraBlockedNets = func() []*net.IPNet {
	out := make([]*net.IPNet, 0, len(extraBlocked))
	for _, c := range extraBlocked {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// IsPublicIP reports whether ip is routable on the public internet. It rejects
// loopback, private, link-local (which covers the cloud metadata endpoints),
// multicast, unspecified and the reserved ranges net.IP.IsPrivate omits.
func IsPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	for _, n := range extraBlockedNets {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// ValidateURL rejects anything that is not an absolute http(s) URL with a host.
// It does not resolve DNS — the dialer guard below is what actually enforces
// the address, so a name that resolves to a private address still fails at
// connect time rather than passing a hostname-only check.
func ValidateURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("only http(s) URLs are allowed (got %q)", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return fmt.Errorf("url is missing a host")
	}
	switch host {
	case "localhost", "ip6-localhost", "ip6-loopback":
		return fmt.Errorf("refusing to reach %s", host)
	}
	if strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") ||
		!strings.Contains(host, ".") && net.ParseIP(host) == nil {
		return fmt.Errorf("refusing to reach internal host %q", host)
	}
	if ip := net.ParseIP(host); ip != nil && !IsPublicIP(ip) {
		return fmt.Errorf("refusing to reach non-public address %s", host)
	}
	return nil
}

// ErrBlockedAddress is returned by the guarded dialer when a connection
// resolves to an address that is not publicly routable.
type ErrBlockedAddress struct{ Addr string }

func (e *ErrBlockedAddress) Error() string {
	return "refusing to connect to non-public address " + e.Addr
}

// guardedControl runs after DNS resolution and before the socket connects, so
// a hostname that resolves to a private or metadata address is rejected here
// rather than at the hostname check. This is what closes DNS rebinding.
func guardedControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("unparsable address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !IsPublicIP(ip) {
		return &ErrBlockedAddress{Addr: host}
	}
	return nil
}

// SafeTransport returns an http.Transport that will only connect to publicly
// routable addresses. Share one per client; it carries its own pool.
func SafeTransport() *http.Transport {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: guardedControl}
	return &http.Transport{
		DialContext:           d.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// SafeClient returns a client that validates every URL it is given (including
// each redirect hop) and refuses to connect to non-public addresses.
// maxRedirects of 0 disables redirect following.
func SafeClient(timeout time.Duration, maxRedirects int) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: SafeTransport(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("too many redirects")
			}
			return ValidateURL(req.URL.String())
		},
	}
}

// Dial is exposed for callers that need the guard outside an http.Client.
func Dial(ctx context.Context, network, address string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: guardedControl}
	return d.DialContext(ctx, network, address)
}
