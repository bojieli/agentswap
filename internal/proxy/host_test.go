package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bojieli/agentswap/internal/config"
	"github.com/bojieli/agentswap/internal/store"
)

func TestHostAllowed(t *testing.T) {
	s := &Server{Config: config.Config{
		Addr:         "127.0.0.1:8420",
		AllowedHosts: []string{"agentswap.internal"},
	}}

	allowed := []string{
		"127.0.0.1:8420",
		"127.0.0.1",
		"localhost:8420",
		"LOCALHOST:8420",
		"[::1]:8420",
		"::1",
		"127.0.0.2:8420", // the whole 127/8 block is this machine
		"foo.localhost:8420",
		"agentswap.internal:8420",
		"", // HTTP/2 and other protocols that do not require a Host
	}
	for _, h := range allowed {
		if !s.hostAllowed(h) {
			t.Errorf("hostAllowed(%q) = false, want true", h)
		}
	}

	// Every one of these is a name an attacker controls, pointed at 127.0.0.1.
	refused := []string{
		"evil.example.com:8420",
		"evil.example.com",
		"attacker.test:8420",
		"192.168.1.10:8420",
		"localhost.evil.example.com:8420",
	}
	for _, h := range refused {
		if s.hostAllowed(h) {
			t.Errorf("hostAllowed(%q) = true, want false", h)
		}
	}
}

// A user who deliberately binds elsewhere must still be able to reach agentswap
// by the address they chose.
func TestHostAllowedFollowsTheBoundAddress(t *testing.T) {
	s := &Server{Config: config.Config{Addr: "192.168.1.10:8420"}}

	if !s.hostAllowed("192.168.1.10:8420") {
		t.Error("the configured listen address is refused as a Host")
	}
	if s.hostAllowed("evil.example.com:8420") {
		t.Error("an unrelated hostname is accepted")
	}
}

// The whole attack is that a page the user visits resolves its own domain to
// 127.0.0.1 and then spends their subscription. The Host header is the one
// thing it cannot forge.
func TestGuardHostRefusesRebinding(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.Upsert(&store.Account{
		ID: "secret-work-account", Lane: store.LaneAnthropic, Kind: store.KindAPIKey,
		Enabled: true, APIKey: "k",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cfg := config.Default()
	s := &Server{
		Store:  st,
		Config: cfg,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	h := s.Handler()

	for _, path := range []string{"/_agentswap/status", "/anthropic/v1/messages"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "evil.example.com"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("%s: status = %d, want %d", path, rec.Code, http.StatusMisdirectedRequest)
		}
		if strings.Contains(rec.Body.String(), "secret-work-account") {
			t.Errorf("%s: refusal leaked the account list: %s", path, rec.Body.String())
		}
	}
}

func TestHostname(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:8420": "127.0.0.1",
		"127.0.0.1":      "127.0.0.1",
		"[::1]:8420":     "::1",
		"[::1]":          "::1",
		"localhost":      "localhost",
		"":               "",
	}
	for in, want := range cases {
		if got := hostname(in); got != want {
			t.Errorf("hostname(%q) = %q, want %q", in, got, want)
		}
	}
}
