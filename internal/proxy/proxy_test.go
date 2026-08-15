package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/agentswap/internal/config"
	"github.com/bojieli/agentswap/internal/engine"
	"github.com/bojieli/agentswap/internal/lane"
	"github.com/bojieli/agentswap/internal/lane/anthropic"
	"github.com/bojieli/agentswap/internal/lane/openai"
	"github.com/bojieli/agentswap/internal/store"
)

// newProxy wires a proxy to a scripted upstream, with one account per lane.
func newProxy(t *testing.T, upstream http.HandlerFunc) *httptest.Server {
	t.Helper()

	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for _, l := range []store.LaneID{store.LaneAnthropic, store.LaneOpenAI} {
		if err := st.Upsert(&store.Account{
			ID: string(l) + "-1", Lane: l, Kind: store.KindAPIKey,
			Label: string(l) + "-1", Enabled: true, APIKey: "k", BaseURL: up.URL,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	cfg := config.Default()
	lanes := map[store.LaneID]lane.Lane{
		store.LaneAnthropic: anthropic.New(up.Client()),
		store.LaneOpenAI:    openai.New(up.Client()),
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Server{
		Engine:    engine.New(cfg, st, lanes, up.Client(), log),
		Store:     st,
		Config:    cfg,
		Keepalive: KeepaliveSilent,
		Log:       log,
	}
	px := httptest.NewServer(s.Handler())
	t.Cleanup(px.Close)
	return px
}

func TestRoutesLanePrefixToUpstreamPath(t *testing.T) {
	var gotPath, gotKey string
	px := newProxy(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey = r.URL.Path, r.Header.Get("X-Api-Key")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	resp, err := http.Post(px.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	// The lane prefix is ours; the upstream must never see it.
	if gotPath != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", gotPath)
	}
	// The pool's credential must be what reaches the upstream.
	if gotKey != "k" {
		t.Errorf("upstream api key = %q, want k", gotKey)
	}
}

func TestOpenAILanePathIsRerooted(t *testing.T) {
	var gotPath string
	px := newProxy(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	resp, err := http.Post(px.URL+"/openai/responses", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if gotPath != "/responses" {
		t.Errorf("upstream path = %q, want /responses", gotPath)
	}
}

// The proxy must forward the client's own credential-free headers and strip
// nothing the upstream needs.
func TestClientHeadersAreForwarded(t *testing.T) {
	var version, beta string
	px := newProxy(t, func(w http.ResponseWriter, r *http.Request) {
		version, beta = r.Header.Get("Anthropic-Version"), r.Header.Get("Anthropic-Beta")
		_, _ = io.WriteString(w, `{}`)
	})

	req, _ := http.NewRequest(http.MethodPost, px.URL+"/anthropic/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "context-1m-2025-08-07")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if version != "2023-06-01" {
		t.Errorf("anthropic-version = %q", version)
	}
	if !strings.Contains(beta, "context-1m-2025-08-07") {
		t.Errorf("anthropic-beta = %q", beta)
	}
}

// Streaming must arrive incrementally; buffering it to the end would stall the
// agent's output even when everything is working.
func TestSSEIsStreamedIncrementally(t *testing.T) {
	release := make(chan struct{})
	px := newProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		rc := http.NewResponseController(w)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		_ = rc.Flush()
		<-release
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		_ = rc.Flush()
	})

	resp, err := http.Post(px.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	first := make(chan string, 1)
	go func() {
		line, _ := br.ReadString('\n')
		first <- line
	}()

	select {
	case line := <-first:
		if !strings.Contains(line, "message_start") {
			t.Errorf("first line = %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first event never arrived; the response is being buffered")
	}
	close(release)
}

func TestUnroutedPathExplainsItself(t *testing.T) {
	px := newProxy(t, func(w http.ResponseWriter, r *http.Request) {})
	resp, err := http.Get(px.URL + "/v1/messages") // missing the lane prefix
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "agentswap install") {
		t.Errorf("404 should point at the fix, got %q", body)
	}
}

func TestStatusEndpointReportsAccounts(t *testing.T) {
	px := newProxy(t, func(w http.ResponseWriter, r *http.Request) {})
	resp, err := http.Get(px.URL + "/_agentswap/status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	var out struct {
		Accounts []AccountStatus `json:"accounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Accounts) != 2 {
		t.Fatalf("got %d accounts, want 2", len(out.Accounts))
	}
}

// Exhaustion past max_hold must produce an actionable 503, not a hang.
func TestExhaustionBeyondMaxHoldReturns503(t *testing.T) {
	px := newProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Retry-After", "18000")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
	})

	resp, err := http.Post(px.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("503 must carry Retry-After so a caller knows when to come back")
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "agentswap run") {
		t.Errorf("503 should name the recovery path, got %q", body)
	}
}

// A client that hangs up must stop the retry loop rather than leave it
// spending quota on a response nobody will read.
func TestClientDisconnectAbortsRetries(t *testing.T) {
	hits := make(chan struct{}, 100)
	px := newProxy(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case hits <- struct{}{}:
		default:
		}
		w.WriteHeader(529)
	})

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, px.URL+"/anthropic/v1/messages", strings.NewReader(`{}`))
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("expected the cancelled request to fail")
	}

	before := len(hits)
	time.Sleep(1500 * time.Millisecond)
	if after := len(hits); after > before+1 {
		t.Errorf("upstream kept being retried after the client left: %d -> %d", before, after)
	}
}

// The client's error message is the whole notification: someone inside a
// coding agent is not watching the daemon's log, and a rejected credential is
// the one failure that will not fix itself.
func TestRejectedMessageIsActionable(t *testing.T) {
	one := rejectedMessage(store.LaneAnthropic, []engine.Rejected{
		{ID: "work", Display: "work", Reason: "refresh failed with 401 Unauthorized"},
	})
	for _, want := range []string{
		`"work"`,                    // which account
		"refresh failed with 401",   // why
		"agentswap login --id work", // the command that fixes it
	} {
		if !strings.Contains(one, want) {
			t.Errorf("message %q does not mention %q", one, want)
		}
	}
	// `import` re-reads the credential the upstream just refused, so it would
	// look like the fix did nothing.
	if strings.Contains(one, "agentswap import") {
		t.Errorf("message recommends import, which cannot help: %q", one)
	}

	many := rejectedMessage(store.LaneAnthropic, []engine.Rejected{
		{ID: "personal", Display: "personal"},
		{ID: "work", Display: "work"},
	})
	for _, want := range []string{"personal", "work",
		"agentswap login --id personal", "agentswap login --id work"} {
		if !strings.Contains(many, want) {
			t.Errorf("message %q does not mention %q", many, want)
		}
	}
}

// "Rejected" and "out of quota" need opposite responses from the user, so they
// must not arrive as the same message.
func TestRejectedPoolIsNotReportedAsAnEmptyOne(t *testing.T) {
	px := newProxy(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"authentication_error"}}`)
	})

	resp, err := http.Post(px.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	got := string(body)
	if !strings.Contains(got, "rejected") {
		t.Errorf("body = %s, want it to say the credential was rejected", got)
	}
	if strings.Contains(got, "no accounts configured") {
		t.Errorf("body = %s, want it to not claim the pool is empty", got)
	}
	if !strings.Contains(got, "agentswap login") {
		t.Errorf("body = %s, want the command that fixes it", got)
	}
}
