package engine

import (
	"net/http"
	"strings"
)

// hopByHop headers govern a single transport connection and must not be
// forwarded to the upstream one.
var hopByHop = map[string]bool{
	"connection":          true,
	"proxy-connection":    true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

func isHopByHop(k string) bool { return hopByHop[strings.ToLower(k)] }

// joinPath appends the client's request path to the upstream's base path,
// collapsing the seam so "/backend-api/codex" + "/responses" does not become
// "/backend-api/codex//responses".
func joinPath(base, req string) string {
	base = strings.TrimSuffix(base, "/")
	if req == "" {
		return base
	}
	if !strings.HasPrefix(req, "/") {
		req = "/" + req
	}
	return base + req
}

// CopyHeader copies src into dst, skipping hop-by-hop headers.
func CopyHeader(dst, src http.Header) {
	for k, vs := range src {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
