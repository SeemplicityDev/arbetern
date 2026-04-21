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

func clientIP(r *http.Request) string {
	// X-Forwarded-For can contain multiple IPs; the first is the original client.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if idx := strings.Index(xff, ","); idx != -1 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}
	// Fall back to direct connection IP.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
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
