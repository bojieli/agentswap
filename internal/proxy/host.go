package proxy

import (
	"net"
	"net/http"
	"strings"
)

// hostAllowed reports whether a request's Host header names this proxy, as
// opposed to some other name that merely resolves to it.
//
// This is the defence against DNS rebinding. A page the user visits cannot be
// stopped from sending requests to 127.0.0.1, and agentswap answers those with
// real subscription credentials — someone else's tokens spending someone else's
// quota. What rebinding cannot do is change the Host header: it arrives as the
// attacker's own domain. Refusing names we do not recognise closes the hole
// without asking the user to configure anything.
func (s *Server) hostAllowed(host string) bool {
	// Go's HTTP server already rejects an HTTP/1.1 request with no Host, and
	// HTTP/2 fills it from :authority. An empty value here is a non-browser
	// client on a protocol where none is required.
	if host == "" {
		return true
	}

	name := hostname(host)
	if name == "" {
		return false
	}
	if strings.EqualFold(name, "localhost") || strings.HasSuffix(strings.ToLower(name), ".localhost") {
		return true
	}
	if ip := net.ParseIP(name); ip != nil && ip.IsLoopback() {
		return true
	}
	// The address we were told to listen on is by definition a name for us.
	if bound := hostname(s.Config.Addr); bound != "" && strings.EqualFold(name, bound) {
		return true
	}
	for _, allowed := range s.Config.AllowedHosts {
		if strings.EqualFold(name, hostname(allowed)) {
			return true
		}
	}
	return false
}

// hostname strips a port and any IPv6 brackets, tolerating input that has
// neither.
func hostname(hostport string) string {
	if hostport == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

// guardHost rejects requests whose Host header we do not recognise.
func (s *Server) guardHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			s.Log.Warn("refused a request for an unrecognised host",
				"host", r.Host, "path", r.URL.Path,
				"hint", "a browser page cannot set this; if it is yours, add it to allowed_hosts")
			writeJSONError(w, http.StatusMisdirectedRequest, "bad_host",
				"agentswap does not answer to host "+r.Host+
					". If you reach it by this name on purpose, add it to allowed_hosts in config.json.")
			return
		}
		next.ServeHTTP(w, r)
	})
}
