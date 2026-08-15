package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// pool seeds n anthropic API-key accounts pointed at the fake upstream.
func (e *env) pool(ids ...string) {
	e.t.Helper()
	for i, id := range ids {
		e.mustRun("add-key", "anthropic",
			"--key", id, "--id", id, "--base-url", e.upstream.url(),
			"--priority", fmt.Sprint(i))
	}
}

func TestProxyForwardsWithThePooledCredential(t *testing.T) {
	e := newEnv(t)
	e.pool("first")
	d := e.serve()

	resp, body := d.post(t, "/anthropic/v1/messages", `{"model":"claude-opus-5"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	seen := e.upstream.seen()
	if len(seen) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(seen))
	}
	// The lane prefix is ours and must not reach the upstream.
	if seen[0].Path != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", seen[0].Path)
	}
	// The pool's credential is what must be used, not the client's.
	if seen[0].Key != "first" {
		t.Errorf("upstream credential = %q, want the pooled one", seen[0].Key)
	}
	if seen[0].Body != `{"model":"claude-opus-5"}` {
		t.Errorf("upstream body = %q, want it forwarded verbatim", seen[0].Body)
	}
}

// Whatever the client sends must be discarded, or a token could leak from one
// configured CLI into another lane.
func TestProxyReplacesTheClientCredential(t *testing.T) {
	e := newEnv(t)
	e.pool("pooled")
	d := e.serve()

	d.postWith(t, "/anthropic/v1/messages", `{}`, map[string]string{
		"X-Api-Key":     "sk-ant-the-clients-own-key",
		"Authorization": "Bearer the-clients-own-token",
	}, 30*time.Second)

	seen := e.upstream.seen()
	if len(seen) != 1 {
		t.Fatalf("upstream saw %d requests", len(seen))
	}
	if seen[0].Key != "pooled" {
		t.Errorf("upstream credential = %q, want the pooled one", seen[0].Key)
	}
	for _, h := range []string{"X-Api-Key", "Authorization"} {
		if v := seen[0].Header.Get(h); strings.Contains(v, "clients-own") {
			t.Errorf("%s = %q: the client's own credential reached the upstream", h, v)
		}
	}
}

// The headline feature: a spent account rotates and the caller never sees the
// 429.
func TestRotationIsInvisibleToTheClient(t *testing.T) {
	e := newEnv(t)
	e.pool("spent", "fresh")
	e.upstream.handle(func(w http.ResponseWriter, _ *http.Request, req recorded) {
		if req.Key == "spent" {
			w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	d := e.serve()

	resp, body := d.post(t, "/anthropic/v1/messages", `{"model":"claude"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s: rotation should be invisible", resp.StatusCode, body)
	}
	if got := e.upstream.keys(); len(got) != 2 || got[0] != "spent" || got[1] != "fresh" {
		t.Errorf("upstream calls = %v, want [spent fresh]", got)
	}

	// The spent account must be remembered, so the next request does not
	// rediscover the limit.
	status := e.mustRun("status").out()
	mustContain(t, status, "exhausted", "status")

	e.upstream.reset()
	if resp, _ := d.post(t, "/anthropic/v1/messages", `{"model":"claude"}`); resp.StatusCode != 200 {
		t.Errorf("second request status = %d", resp.StatusCode)
	}
	if got := e.upstream.keys(); len(got) != 1 || got[0] != "fresh" {
		t.Errorf("second request went to %v, want only [fresh]", got)
	}
}

// An overloaded upstream is temporary, and surfacing it is what strands the
// agent mid-task.
func TestOverloadIsRetriedNotSurfaced(t *testing.T) {
	e := newEnv(t)
	e.pool("only")
	var calls int
	var mu sync.Mutex
	e.upstream.handle(func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n < 3 {
			w.WriteHeader(529)
			_, _ = io.WriteString(w, `{"error":{"type":"overloaded_error"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	d := e.serve()

	resp, body := d.post(t, "/anthropic/v1/messages", `{"model":"claude"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s: overload should be absorbed", resp.StatusCode, body)
	}
	if len(e.upstream.seen()) < 3 {
		t.Errorf("upstream saw %d requests, want the retries", len(e.upstream.seen()))
	}
}

// When everything is spent the request waits rather than failing, and the
// client sees only the eventual success.
func TestParkingWaitsForQuotaAndThenSucceeds(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.home, "config.json"),
		`{"park":{"enabled":true,"buffer":"1s","max_hold":"2m"}}`)
	e.pool("only")

	var mu sync.Mutex
	spent := true
	e.upstream.handle(func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		mu.Lock()
		isSpent := spent
		mu.Unlock()
		if isSpent {
			w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			w.Header().Set("Retry-After", "4")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	d := e.serve()

	go func() {
		time.Sleep(3 * time.Second)
		mu.Lock()
		spent = false
		mu.Unlock()
	}()

	start := time.Now()
	resp, body := d.post(t, "/anthropic/v1/messages", `{"model":"claude"}`)
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d after %v, body = %s", resp.StatusCode, elapsed, body)
	}
	if elapsed < 2*time.Second {
		t.Errorf("returned after %v, which is too fast to have waited for the reset", elapsed)
	}
}

// Past max_hold, holding the socket is worse than saying when to come back.
func TestParkingBeyondMaxHoldHandsOff(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.home, "config.json"),
		`{"park":{"enabled":true,"buffer":"1s","max_hold":"2s"}}`)
	e.pool("only")
	e.upstream.handle(func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
	})
	d := e.serve()

	resp, body := d.post(t, "/anthropic/v1/messages", `{"model":"claude"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503\n%s", resp.StatusCode, body)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After: the client is not told when to come back")
	}
	// The error has to say what to do about it.
	mustContain(t, body, "agentswap run", "503 body")

	// And it must leave a ticket, which is what makes the resume work.
	ticket := filepath.Join(e.home, "pending", "anthropic.json")
	if _, err := os.Stat(ticket); err != nil {
		t.Errorf("no resume ticket written: %v", err)
	}
}

func TestNoAccountsIsAnActionableError(t *testing.T) {
	e := newEnv(t)
	d := e.serve()

	resp, body := d.post(t, "/anthropic/v1/messages", `{}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	mustContain(t, body, "agentswap import", "no accounts")
	// The daemon should have said so at startup rather than waiting for this.
	mustContain(t, d.log.String(), "lane has no accounts", "startup log")
}

// A model streams for minutes; tokens have to arrive as they are produced.
func TestStreamingIsIncremental(t *testing.T) {
	e := newEnv(t)
	e.pool("only")
	e.upstream.handle(func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "event: chunk\ndata: {\"n\":%d}\n\n", i)
			flusher.Flush()
			time.Sleep(300 * time.Millisecond)
		}
	})
	d := e.serve()

	req, _ := http.NewRequest(http.MethodPost, "http://"+d.addr+"/anthropic/v1/messages",
		strings.NewReader(`{"stream":true}`))
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want it forwarded", ct)
	}

	start := time.Now()
	var arrivals []time.Duration
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data:") {
			arrivals = append(arrivals, time.Since(start))
		}
	}
	if len(arrivals) != 3 {
		t.Fatalf("received %d chunks, want 3", len(arrivals))
	}
	// Buffering would make every chunk arrive at once, at the end.
	if arrivals[2]-arrivals[0] < 300*time.Millisecond {
		t.Errorf("chunks arrived %v apart: the response was buffered, not streamed", arrivals[2]-arrivals[0])
	}
}

// A request the client got wrong must go straight back, not burn another
// account's quota failing identically.
func TestClientErrorsPassStraightThrough(t *testing.T) {
	e := newEnv(t)
	e.pool("first", "second")
	e.upstream.handle(func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"bad model"}}`)
	})
	d := e.serve()

	resp, body := d.post(t, "/anthropic/v1/messages", `{"model":"nope"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 relayed unchanged", resp.StatusCode)
	}
	mustContain(t, body, "bad model", "relayed error body")
	if n := len(e.upstream.seen()); n != 1 {
		t.Errorf("upstream saw %d requests, want 1: a bad request must not be retried", n)
	}
}

func TestUnroutedPathExplainsItself(t *testing.T) {
	e := newEnv(t)
	e.pool("only")
	d := e.serve()

	resp, err := http.Get("http://" + d.addr + "/v1/messages")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	mustContain(t, string(body), "/anthropic", "unrouted path")
}

// A page you visit cannot be stopped from posting to 127.0.0.1, but it cannot
// forge the Host header either.
func TestDNSRebindingIsRefused(t *testing.T) {
	e := newEnv(t)
	e.pool("only")
	d := e.serve()

	resp, body := d.postWith(t, "/anthropic/v1/messages", `{}`,
		map[string]string{"Host": "evil.example.com"}, 10*time.Second)

	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("status = %d, want 421", resp.StatusCode)
	}
	if n := len(e.upstream.seen()); n != 0 {
		t.Errorf("upstream saw %d requests: a refused request must not spend quota", n)
	}
	mustNotContain(t, body, "only", "refusal body")
}

func TestStatusReportsLiveQuota(t *testing.T) {
	e := newEnv(t)
	e.pool("only")
	e.upstream.handle(func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "42")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", "2099-01-01T00:00:00Z")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	d := e.serve()
	d.post(t, "/anthropic/v1/messages", `{}`)

	// Reading quota off a successful response is what makes rotating before a
	// 429 possible, so it has to reach `status` without waiting for a flush.
	out := e.mustRun("status").out()
	mustContain(t, out, "42%", "status")
	mustNotContain(t, out, "daemon not running", "status")
}

// An account added while the daemon is up is not in the daemon's answer, and
// must not render as a blank row.
func TestStatusShowsAccountsAddedAfterStartup(t *testing.T) {
	e := newEnv(t)
	e.pool("first")
	e.serve()

	e.mustRun("add-key", "anthropic", "--key", "later", "--id", "later")
	out := e.mustRun("status").out()

	mustContain(t, out, "later", "status")
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "later") && !strings.Contains(line, "available") {
			t.Errorf("new account row has no state: %q", line)
		}
	}
}

func TestHealthSurvivesADaemonRestart(t *testing.T) {
	if !gracefulStopSupported() {
		t.Skip("cannot deliver Ctrl-C to a child process here")
	}
	e := newEnv(t)
	e.pool("spent", "fresh")
	e.upstream.handle(func(w http.ResponseWriter, _ *http.Request, req recorded) {
		if req.Key == "spent" {
			w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	d := e.serve()
	d.post(t, "/anthropic/v1/messages", `{}`)
	d.stop(t)

	// Re-probing an account we already know is spent wastes a request and
	// re-triggers the limit.
	state := readFile(t, filepath.Join(e.home, "state.json"))
	if !strings.Contains(state, "exhausted") {
		t.Fatalf("state was not flushed on shutdown:\n%s", state)
	}

	e.upstream.reset()
	d2 := e.serve()
	d2.post(t, "/anthropic/v1/messages", `{}`)

	if got := e.upstream.keys(); len(got) != 1 || got[0] != "fresh" {
		t.Errorf("after restart the upstream saw %v, want only [fresh]", got)
	}
}

// A clean shutdown should not leave other commands chasing a daemon that is
// gone.
func TestShutdownClearsThePublishedAddress(t *testing.T) {
	if !gracefulStopSupported() {
		t.Skip("cannot deliver Ctrl-C to a child process here")
	}
	e := newEnv(t)
	e.pool("only")
	d := e.serve()
	d.stop(t)

	if _, err := os.Stat(filepath.Join(e.home, "daemon.json")); !os.IsNotExist(err) {
		t.Errorf("daemon.json survived a clean shutdown: %v", err)
	}
	mustContain(t, e.mustRun("status").out(), "daemon not running", "status after shutdown")
}

func TestConcurrentRequestsAreAllServed(t *testing.T) {
	e := newEnv(t)
	e.pool("only")
	e.upstream.handle(func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		time.Sleep(50 * time.Millisecond)
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	d := e.serve()

	const n = 12
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, _ := d.postWith(t, "/anthropic/v1/messages",
				fmt.Sprintf(`{"n":%d}`, i), nil, 30*time.Second)
			codes[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request %d: status %d", i, c)
		}
	}
	if got := len(e.upstream.seen()); got != n {
		t.Errorf("upstream saw %d requests, want %d", got, n)
	}
}

func TestOpenAILaneIsRouted(t *testing.T) {
	e := newEnv(t)
	e.mustRun("add-key", "openai", "--key", "sk-proj-test", "--base-url", e.upstream.url())
	d := e.serve()

	resp, body := d.post(t, "/openai/responses", `{"model":"gpt-5.6"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	seen := e.upstream.seen()
	if len(seen) != 1 || seen[0].Path != "/responses" {
		t.Fatalf("upstream saw %+v, want /responses", seen)
	}
	if seen[0].Key != "sk-proj-test" {
		t.Errorf("credential = %q, want the pooled key", seen[0].Key)
	}
}

func TestAdminStatusEndpointShape(t *testing.T) {
	e := newEnv(t)
	e.mustRun("add-key", "anthropic", "--key", "only-secret", "--id", "only", "--base-url", e.upstream.url())
	d := e.serve()

	resp, err := http.Get("http://" + d.addr + "/_agentswap/status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var out struct {
		Accounts []struct {
			ID     string          `json:"id"`
			Health json.RawMessage `json:"health"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("status endpoint is not valid json: %v\n%s", err, body)
	}
	if len(out.Accounts) != 1 || out.Accounts[0].ID != "only" {
		t.Errorf("accounts = %+v", out.Accounts)
	}
	// Quota and health are fine to expose on loopback; credentials are not.
	// The account's *kind* is legitimately the string "api_key", so look for
	// the fields that would carry a secret and for the secret itself.
	for _, leak := range []string{`"api_key":`, `"access_token":`, `"refresh_token":`, "only-secret"} {
		if strings.Contains(string(body), leak) {
			t.Errorf("status endpoint leaks %s:\n%s", leak, body)
		}
	}
}

// A daemon that was killed rather than asked to stop leaves its address behind.
// Every other command has to notice that nothing answers there and fall back,
// on every platform — this is what a hard kill or a crash actually looks like.
func TestStaleDaemonAddressDoesNotBreakStatus(t *testing.T) {
	e := newEnv(t)
	e.pool("only")

	dead := freePort(t)
	writeFile(t, filepath.Join(e.home, "daemon.json"),
		`{"addr":"`+dead+`","pid":999999,"version":"dev","started_at":"2026-01-01T00:00:00Z"}`)

	out := e.mustRun("status").out()
	mustContain(t, out, "daemon not running", "status with a stale address")
	mustContain(t, out, "only", "status with a stale address")

	r := e.run("doctor")
	if r.code == 0 {
		t.Error("doctor passed while pointed at a daemon that is not there")
	}
	mustContain(t, r.out(), "agentswap serve", "doctor with a stale address")
}

// And once a daemon really is running, the stale entry must not keep other
// commands looking at the wrong place.
func TestStaleDaemonAddressIsReplacedOnStartup(t *testing.T) {
	e := newEnv(t)
	e.pool("only")
	writeFile(t, filepath.Join(e.home, "daemon.json"),
		`{"addr":"`+freePort(t)+`","pid":999999,"version":"dev","started_at":"2026-01-01T00:00:00Z"}`)

	d := e.serve()
	if got := e.publishedAddr(); got != d.addr {
		t.Errorf("published address = %q, want the running daemon's %q", got, d.addr)
	}
	mustNotContain(t, e.mustRun("status").out(), "daemon not running", "status")
}

// A second daemon on a taken port must fail without disturbing the first: it
// has to exit before publishing an address, or `status` would start chasing a
// process that never started.
func TestASecondDaemonFailsWithoutClobberingTheFirst(t *testing.T) {
	e := newEnv(t)
	e.pinAddr()
	e.pool("only")
	first := e.serve()

	second := exec.Command(binary, "serve")
	second.Env = e.environ()
	out, err := second.CombinedOutput()
	if err == nil {
		t.Fatal("a second daemon bound a port that was already taken")
	}
	if !strings.Contains(string(out), "address already in use") &&
		!strings.Contains(string(out), "Only one usage") {
		t.Errorf("unhelpful error for a taken port:\n%s", out)
	}

	if got := e.publishedAddr(); got != first.addr {
		t.Errorf("published address = %q, want the running daemon's %q", got, first.addr)
	}
	mustNotContain(t, e.mustRun("status").out(), "daemon not running", "status")
}

// systemd and launchd stop a service with SIGTERM, not Ctrl-C.
func TestSigtermShutsDownCleanly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no SIGTERM here")
	}
	e := newEnv(t)
	e.pool("only")
	d := e.serve()
	d.post(t, "/anthropic/v1/messages", `{}`)

	if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- d.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the daemon ignored SIGTERM")
	}

	if _, err := os.Stat(filepath.Join(e.home, "daemon.json")); !os.IsNotExist(err) {
		t.Error("daemon.json survived a SIGTERM shutdown")
	}
	mustContain(t, readFile(t, filepath.Join(e.home, "state.json")), "requests", "state after SIGTERM")
}
