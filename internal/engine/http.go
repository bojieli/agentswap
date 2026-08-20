package engine

import (
	"bytes"
	"io"
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

// readHead samples the start of a response body: enough to hold the first
// stream event or error envelope, no more. Reading stops at the first blank
// line — the event-stream boundary — so a healthy stream, which emits its
// first event immediately, pays no added latency, while an upstream that
// fails in-band sends that failure as the first event and is caught.
func readHead(r io.Reader) []byte {
	var buf []byte
	tmp := make([]byte, 4<<10)
	for len(buf) < headSampleLimit {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil || n == 0 || bytes.Contains(buf, []byte("\n\n")) {
			break
		}
	}
	return buf
}

// prefixReadCloser reattaches a sampled head to the rest of an open body.
type prefixReadCloser struct {
	io.Reader
	io.Closer
}

// prefixBody returns a body that yields head and then the remainder of rest,
// so classification can peek without the relay losing a byte.
func prefixBody(head []byte, rest io.ReadCloser) io.ReadCloser {
	if len(head) == 0 {
		return rest
	}
	return prefixReadCloser{io.MultiReader(bytes.NewReader(head), rest), rest}
}
