package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A disabled account is out of rotation, not merely hidden from `list`.
func TestDisabledAccountIsNotUsed(t *testing.T) {
	e := newEnv(t)
	e.pool("off", "on")
	e.mustRun("disable", "off")
	d := e.serve()

	d.post(t, "/anthropic/v1/messages", `{}`)

	if got := e.upstream.keys(); len(got) != 1 || got[0] != "on" {
		t.Errorf("upstream saw %v, want only the enabled account", got)
	}
}

func TestPriorityDecidesOrder(t *testing.T) {
	e := newEnv(t)
	e.mustRun("add-key", "anthropic", "--key", "second", "--id", "second",
		"--base-url", e.upstream.url(), "--priority", "10")
	e.mustRun("add-key", "anthropic", "--key", "first", "--id", "first",
		"--base-url", e.upstream.url(), "--priority", "1")
	d := e.serve()

	d.post(t, "/anthropic/v1/messages", `{}`)

	if got := e.upstream.keys(); len(got) != 1 || got[0] != "first" {
		t.Errorf("upstream saw %v, want the lower priority number first", got)
	}
}

// Subscriptions are already paid for, so they are spent before metered keys
// whatever the priority says.
func TestSubscriptionsAreSpentBeforeAPIKeys(t *testing.T) {
	e := newEnv(t)
	claudeLogin(t, e, "subscription-token")
	e.mustRun("import", "--id", "sub")
	// Point the imported subscription at the fake upstream.
	e.mustRun("add-key", "anthropic", "--key", "metered", "--id", "metered",
		"--base-url", e.upstream.url(), "--priority", "0")

	// The subscription has no base_url, so give it one by re-importing under a
	// configured gateway: the ordering is what is under test, so both accounts
	// have to reach the fake upstream.
	writeFile(t, filepath.Join(e.home, "accounts.json"), fmt.Sprintf(`[
	  {"id":"sub","lane":"anthropic","kind":"oauth","label":"sub","priority":100,
	   "enabled":true,"access_token":"subscription-token","expires_at":4102444800000,
	   "base_url":%q},
	  {"id":"metered","lane":"anthropic","kind":"api_key","label":"metered","priority":0,
	   "enabled":true,"api_key":"metered","base_url":%q}
	]`, e.upstream.url(), e.upstream.url()))

	d := e.serve()
	d.post(t, "/anthropic/v1/messages", `{}`)

	if got := e.upstream.keys(); len(got) != 1 || got[0] != "subscription-token" {
		t.Errorf("upstream saw %v, want the subscription first despite its worse priority", got)
	}
}

// With parking off, agentswap fails fast instead of holding the request.
func TestParkingDisabledFailsImmediately(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.home, "config.json"), `{"park":{"enabled":false}}`)
	e.pool("only")
	e.upstream.handle(func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	d := e.serve()

	start := time.Now()
	resp, _ := d.post(t, "/anthropic/v1/messages", `{}`)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %v with parking disabled, want a prompt failure", elapsed)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	// The daemon should have warned about this at startup.
	mustContain(t, d.log.String(), "parking is disabled", "startup log")
}

// A client that goes away must stop the work being done on its behalf.
// Retrying into an overloaded upstream forever, for an answer nobody will
// read, is quota spent on nothing.
func TestClientDisconnectStopsTheRetries(t *testing.T) {
	e := newEnv(t)
	e.pool("only")

	var mu sync.Mutex
	var calls int
	e.upstream.handle(func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(529)
		_, _ = io.WriteString(w, `{"error":{"type":"overloaded_error"}}`)
	})
	d := e.serve()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+d.addr+"/anthropic/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(2 * time.Second)
		cancel()
	}()
	if _, err := (&http.Client{}).Do(req); err == nil {
		t.Fatal("expected the cancelled request to fail")
	}

	mu.Lock()
	atCancel := calls
	mu.Unlock()

	// Give the daemon a moment to notice and stop.
	time.Sleep(3 * time.Second)
	mu.Lock()
	after := calls
	mu.Unlock()

	if after > atCancel+1 {
		t.Errorf("upstream calls went from %d to %d after the client left: the retries did not stop",
			atCancel, after)
	}
}

// Whatever the CLI sends has to arrive unchanged: query strings, repeated
// headers, and bodies that are not ASCII.
func TestRequestIsForwardedFaithfully(t *testing.T) {
	e := newEnv(t)
	e.pool("only")
	d := e.serve()

	body := `{"prompt":"héllo — 世界 🌍","nested":{"a":[1,2,3]}}`
	req, err := http.NewRequest(http.MethodPost,
		"http://"+d.addr+"/anthropic/v1/messages?beta=true&limit=5", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Add("Anthropic-Beta", "one")
	req.Header.Add("Anthropic-Beta", "two")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	seen := e.upstream.seen()
	if len(seen) != 1 {
		t.Fatalf("upstream saw %d requests", len(seen))
	}
	if seen[0].Body != body {
		t.Errorf("body = %q, want it byte for byte", seen[0].Body)
	}
	if seen[0].Header.Get("Anthropic-Version") != "2023-06-01" {
		t.Errorf("Anthropic-Version = %q", seen[0].Header.Get("Anthropic-Version"))
	}
	// Repeated and comma-joined are the same thing to the API, so merging is
	// fine — losing one of them is not.
	betas := strings.Join(seen[0].Header.Values("Anthropic-Beta"), ",")
	for _, want := range []string{"one", "two"} {
		if !strings.Contains(betas, want) {
			t.Errorf("Anthropic-Beta = %q, want it to include %q", betas, want)
		}
	}
}

func TestQueryStringSurvives(t *testing.T) {
	e := newEnv(t)
	e.pool("only")
	var query string
	e.upstream.handle(func(w http.ResponseWriter, r *http.Request, _ recorded) {
		query = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	d := e.serve()

	req, _ := http.NewRequest(http.MethodPost,
		"http://"+d.addr+"/anthropic/v1/messages?beta=true&limit=5", strings.NewReader(`{}`))
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if query != "beta=true&limit=5" {
		t.Errorf("upstream query = %q, want it forwarded", query)
	}
}

// A big context is the normal case for a coding agent, not an attack.
func TestLargeBodyIsForwarded(t *testing.T) {
	e := newEnv(t)
	e.pool("only")
	d := e.serve()

	// A few megabytes, comfortably inside the limit and well past any buffer.
	payload := strings.Repeat("x", 4<<20)
	body := `{"prompt":"` + payload + `"}`

	resp, out := d.postWith(t, "/anthropic/v1/messages", body, nil, 60*time.Second)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, out)
	}
	seen := e.upstream.seen()
	if len(seen) != 1 || len(seen[0].Body) != len(body) {
		t.Errorf("upstream got %d bytes, want %d", len(seen[0].Body), len(body))
	}
}

// An account pointed somewhere unreachable must produce a diagnosable error
// rather than a silent hang.
func TestUnreachableUpstreamIsReported(t *testing.T) {
	e := newEnv(t)
	dead := freePort(t)
	e.mustRun("add-key", "anthropic", "--key", "k", "--id", "dead", "--base-url", "http://"+dead)
	writeFile(t, filepath.Join(e.home, "config.json"),
		`{"retry":{"overload_initial":"100ms","overload_max":"200ms","rotate_after":2},`+
			`"park":{"enabled":false}}`)
	d := e.serve()

	resp, body := d.postWith(t, "/anthropic/v1/messages", `{}`, nil, 60*time.Second)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("status = %d, want a failure", resp.StatusCode)
	}
	mustContain(t, d.log.String(), "upstream request failed", "daemon log")
	if body == "" {
		t.Error("the client got an empty body with no explanation")
	}
}

// The riskier keepalive mode: once the status line is sent it cannot be taken
// back, so what the client receives has to remain well-formed.
func TestPingKeepaliveHoldsTheConnection(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.home, "config.json"),
		`{"park":{"enabled":true,"buffer":"1s","max_hold":"2m","keepalive":"ping","keepalive_interval":"500ms"}}`)
	e.pool("only")

	var mu sync.Mutex
	spent := true
	e.upstream.handle(func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		mu.Lock()
		isSpent := spent
		mu.Unlock()
		if isSpent {
			w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: done\ndata: {\"ok\":true}\n\n")
	})
	d := e.serve()

	go func() {
		time.Sleep(2 * time.Second)
		mu.Lock()
		spent = false
		mu.Unlock()
	}()

	resp, body := d.postWith(t, "/anthropic/v1/messages", `{}`, nil, 60*time.Second)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: ping mode commits to one up front", resp.StatusCode)
	}
	if !strings.Contains(body, "event: ping") {
		t.Errorf("no keepalive frames in:\n%q", body)
	}
	// The real answer still has to arrive after them.
	if !strings.Contains(body, `"ok":true`) {
		t.Errorf("the answer never arrived:\n%q", body)
	}
	// Every SSE frame ends with a blank line, pings included.
	if !strings.HasSuffix(body, "\n\n") {
		t.Errorf("the stream does not end on a frame boundary:\n%q", body)
	}
}
