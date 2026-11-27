package airplayd

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// requireLocalHostAndOrigin blocks DNS rebinding and cross-origin browser
// requests against the localhost UI API.
func requireLocalHostAndOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || !ip.IsLoopback() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if !isAllowedAPIHost(r.Host) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if origins := r.Header.Values("Origin"); len(origins) > 0 {
			if len(origins) > 1 || !isAllowedAPIOrigin(origins[0]) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isAllowedAPIHost(raw string) bool {
	if raw == "" {
		return false
	}
	if strings.Contains(raw, ":") {
		if raw == "[::1]" {
			return true
		}
		host, port, err := net.SplitHostPort(raw)
		if err != nil || !isValidPort(port) {
			return false
		}
		return host == "127.0.0.1" || strings.EqualFold(host, "localhost") || host == "::1"
	}
	return raw == "127.0.0.1" || strings.EqualFold(raw, "localhost")
}

func isAllowedAPIOrigin(origin string) bool {
	// "null" is the opaque origin of sandboxed iframes, data: and file: pages.
	// It is exactly the CSRF vector this check exists to block, so reject it.
	if origin == "null" || origin == "" || strings.ContainsAny(origin, "?#") {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil || !isValidPort(port) {
		return false
	}
	return host == "127.0.0.1" || strings.EqualFold(host, "localhost")
}

func isValidPort(port string) bool {
	if port == "" {
		return false
	}
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}
