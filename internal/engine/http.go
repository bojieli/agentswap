package engine

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"
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

// headProbeTimeout is the maximum time probeHead waits for the first SSE
// event boundary before giving up and relaying the body unchanged. It guards
// against upstreams that stall before their first byte; the happy path costs
// no time because healthy SSE servers emit an opening event immediately.
//
// Set to 0 in tests that measure raw body bytes directly (disables probing).
var headProbeTimeout = 5 * time.Second

// probeHead samples the start of a 2xx body so the lane can classify
// in-band errors before anything reaches the client.
//
// To avoid concurrent reads — which would corrupt the stream — the goroutine
// is the sole reader of rc. It forwards every byte it reads to a pipe so the
// caller always reads from the pipe, never from rc directly. The returned
// sample is for classification only; the returned body delivers the complete
// original stream, including the probed bytes.
//
// If no event boundary (\n\n) arrives within headProbeTimeout the probe
// gives up and returns a nil sample: classification sees nothing, the engine
// relays the body unchanged, and the watchdog adds no latency.
func probeHead(rc io.ReadCloser) (sample []byte, body io.ReadCloser) {
	if headProbeTimeout == 0 {
		return nil, rc
	}

	pr, pw := io.Pipe()
	ch := make(chan []byte, 1) // buffered: goroutine never blocks on send

	go func() {
		// Phase 1: accumulate bytes from rc until we see the first SSE event
		// boundary (\n\n), hit the sample cap, or exhaust the body. We do NOT
		// write to pw yet: io.Pipe is synchronous and pw.Write blocks until
		// someone reads from pr. The caller is blocked in the select below,
		// not reading from pr, so writing before signalling ch would deadlock.
		var buf []byte
		tmp := make([]byte, 4<<10)
		var phase1Err error
		for len(buf) < headSampleLimit {
			n, rerr := rc.Read(tmp)
			buf = append(buf, tmp[:n]...)
			if rerr != nil || n == 0 || bytes.Contains(buf, []byte("\n\n")) {
				phase1Err = rerr
				break
			}
		}

		// Signal the sample. The channel is buffered so this never blocks,
		// even if the caller already gave up due to the timeout.
		ch <- buf

		// Phase 2: the caller now has the sample and has returned pr as the
		// body, so there is (or shortly will be) a reader on pr. Write the
		// accumulated bytes and forward the rest.
		if _, werr := pw.Write(buf); werr != nil {
			// Reader closed (retry path) — drain rc and exit.
			_, _ = io.Copy(io.Discard, rc)
			_ = rc.Close()
			return
		}
		if phase1Err != nil {
			if phase1Err == io.EOF {
				phase1Err = nil
			}
			pw.CloseWithError(phase1Err)
			_ = rc.Close()
			return
		}
		pw.CloseWithError(forwardAll(rc, pw))
		_ = rc.Close()
	}()

	t := time.NewTimer(headProbeTimeout)
	defer t.Stop()

	select {
	case s := <-ch:
		// Goroutine already wrote s (and will write the rest) to pw, so pr
		// delivers the complete stream. Return s for classification and pr
		// as the body — no bytes lost, no duplication.
		return s, pr
	case <-t.C:
		// No event in time. Goroutine continues writing to pw in the
		// background; relay reads from pr and gets the full stream.
		return nil, pr
	}
}

// forwardAll copies src into pw, mapping io.EOF to nil for pw.CloseWithError.
func forwardAll(src io.Reader, pw *io.PipeWriter) error {
	_, err := io.Copy(pw, src)
	if err == io.EOF {
		return nil
	}
	return err
}

// prefixBody returns a body that yields head and then the remainder of rest.
// Used for ≥300 responses where the error body was already read into a []byte.
// For 2xx bodies probeHead handles byte forwarding without copying.
func prefixBody(head []byte, rest io.ReadCloser) io.ReadCloser {
	if len(head) == 0 {
		return rest
	}
	return struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), rest), rest}
}
