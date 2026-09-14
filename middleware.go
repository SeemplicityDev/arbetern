package main

import (
	"log"
	"net"
	"net/http"
	"strings"
)

func parseCIDRs(raw string) []*net.IPNet {
	if raw == "" {
		return nil
	}
	var nets []*net.IPNet
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		// Allow bare IPs like "1.2.3.4" by appending /32 or /128.
		if !strings.Contains(s, "/") {
			if strings.Contains(s, ":") {
				s += "/128"
			} else {
				s += "/32"
			}
		}
		_, cidr, err := net.ParseCIDR(s)
		if err != nil {
			log.Printf("WARNING: ignoring invalid CIDR %q: %v", s, err)
			continue
		}
		nets = append(nets, cidr)
	}
	return nets
}

// trustedProxies are the peers whose X-Forwarded-For and identity headers are
// believed. Set once at startup from TRUSTED_PROXY_CIDRS; empty means no peer
// is trusted and forwarded headers are ignored for IP decisions.
var trustedProxies []*net.IPNet

// setTrustedProxies installs the trusted-proxy list. Call before serving.
func setTrustedProxies(nets []*net.IPNet) { trustedProxies = nets }

func ipInAny(ip net.IP, nets []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// directPeerIP is the address the connection actually came from.
func directPeerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// fromTrustedProxy reports whether the immediate peer is an allowed proxy.
// With no list configured nothing is trusted.
func fromTrustedProxy(r *http.Request) bool {
	if len(trustedProxies) == 0 {
		return false
	}
	return ipInAny(net.ParseIP(directPeerIP(r)), trustedProxies)
}

// clientIP returns the address to make access decisions on. X-Forwarded-For is
// only consulted when the immediate peer is a trusted proxy, and then it is
// walked from the right, skipping trusted hops — a client-supplied prefix
// cannot reach the front of that walk, so the header cannot be spoofed past
// the gate.
func clientIP(r *http.Request) string {
	peer := directPeerIP(r)
	if !fromTrustedProxy(r) {
		return peer
	}
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		h := strings.TrimSpace(hops[i])
		if h == "" {
			continue
		}
		ip := net.ParseIP(h)
		if ip == nil {
			break
		}
		if ipInAny(ip, trustedProxies) {
			continue
		}
		return h
	}
	return peer
}

// globalIPGate returns a middleware that denies every request by default and
// only lets through:
//   - exempt URL paths (exact match or prefix match when the exempt entry ends
//     in "/"), e.g. "/healthz" or Slack slash-command webhooks,
//   - requests whose client IP falls inside one of the allowed CIDRs.
//
// When cidrs is empty the middleware is a no-op — operators who have not
// configured UI_ALLOWED_CIDRS keep the previous open-by-default behaviour.
// This is the coarse-grained equivalent of ipWhitelist and is intended to be
// applied once at the top of the handler chain so dashboards, workflows, UI,
// and API are all covered with a single rule.
func globalIPGate(cidrs []*net.IPNet, exempt []string, next http.Handler) http.Handler {
	if len(cidrs) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isExemptPath(r.URL.Path, exempt) {
			next.ServeHTTP(w, r)
			return
		}
		ip := net.ParseIP(clientIP(r))
		if ip != nil {
			for _, cidr := range cidrs {
				if cidr.Contains(ip) {
					next.ServeHTTP(w, r)
					return
				}
			}
		}
		log.Printf("access denied for IP %s path=%s", clientIP(r), r.URL.Path)
		http.Error(w, "Forbidden", http.StatusForbidden)
	})
}

// isExemptPath reports whether path matches any exemption rule. An exempt
// entry ending in "/" matches as a prefix; otherwise an exact match is
// required. Keeping this strict avoids accidental exposure of endpoints that
// merely share a common prefix with an exempted route.
func isExemptPath(path string, exempt []string) bool {
	for _, e := range exempt {
		if e == "" {
			continue
		}
		if strings.HasSuffix(e, "/") {
			if strings.HasPrefix(path, e) {
				return true
			}
			continue
		}
		if path == e {
			return true
		}
	}
	return false
}
