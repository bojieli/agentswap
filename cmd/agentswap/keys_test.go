package main

import (
	"os"
	"strings"
	"testing"

	"github.com/bojieli/agentswap/internal/store"
)

// A masked key has one job: let someone with several keys tell which row is
// which, without putting a secret on screen or in a scrollback buffer.
func TestMaskKey(t *testing.T) {
	cases := map[string]string{
		"sk-ant-api03-abcdefghijklmnop": "sk-ant-api…mnop",
		"sk-ant-oat01-zzzzzzzzzzzz9999": "sk-ant-oat…9999",
		"sk-proj-abcdefghijkl1234":      "sk-proj…1234",
		"gw-secret-value-5678":          "…5678",
		"tiny":                          "…",
		"":                              "…",
	}
	for key, want := range cases {
		if got := maskKey(key); got != want {
			t.Errorf("maskKey(%q) = %q, want %q", key, got, want)
		}
	}

	// Whatever the format, the middle of the secret must not survive.
	secret := "sk-ant-api03-THISPARTISSECRETXYZ"
	if got := maskKey(secret); strings.Contains(got, "SECRET") {
		t.Errorf("maskKey leaked the key: %q", got)
	}
}

func TestHostOf(t *testing.T) {
	cases := map[string]string{
		"https://llm.corp.example.com/v1": "llm.corp.example.com",
		"http://127.0.0.1:8080":           "127.0.0.1:8080",
		"https://api.anthropic.com":       "api.anthropic.com",
		"":                                "",
	}
	for in, want := range cases {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCredentialSummary(t *testing.T) {
	key := &store.Account{Kind: store.KindAPIKey, APIKey: "sk-ant-api03-abcdefgh1234"}
	if got := credentialSummary(key); !strings.Contains(got, "1234") {
		t.Errorf("summary = %q, want the key's tail so it can be told apart", got)
	}
	if got := credentialSummary(key); strings.Contains(got, "abcdefgh") {
		t.Errorf("summary = %q, leaked the key", got)
	}

	gateway := &store.Account{
		Kind: store.KindAPIKey, APIKey: "gw-1234", BaseURL: "https://llm.corp.example.com/v1",
	}
	if got := credentialSummary(gateway); !strings.Contains(got, "llm.corp.example.com") {
		t.Errorf("summary = %q, want the provider named", got)
	}

	sub := &store.Account{Kind: store.KindOAuth, SubscriptionType: "max", AccessToken: "secret-token"}
	got := credentialSummary(sub)
	if !strings.Contains(got, "max") {
		t.Errorf("summary = %q, want the plan named", got)
	}
	if strings.Contains(got, "secret-token") {
		t.Errorf("summary = %q, leaked the token", got)
	}
}

// A secret on the command line lands in shell history and the process list, so
// every other route has to work.
func TestReadSecretPrefersTheSafeRoutes(t *testing.T) {
	t.Run("flag value", func(t *testing.T) {
		got, err := readSecretT("sk-from-flag")
		if err != nil || got != "sk-from-flag" {
			t.Errorf("= %q, %v", got, err)
		}
	})

	t.Run("environment", func(t *testing.T) {
		t.Setenv("AGENTSWAP_API_KEY", "sk-from-env")
		got, err := readSecretT("")
		if err != nil || got != "sk-from-env" {
			t.Errorf("= %q, %v", got, err)
		}
	})

	t.Run("the flag wins over the environment", func(t *testing.T) {
		t.Setenv("AGENTSWAP_API_KEY", "sk-from-env")
		got, _ := readSecretT("sk-from-flag")
		if got != "sk-from-flag" {
			t.Errorf("= %q, want the explicit value", got)
		}
	})

	t.Run("stdin", func(t *testing.T) {
		withStdin(t, "sk-from-stdin\n")
		got, err := readSecretT("-")
		if err != nil || got != "sk-from-stdin" {
			t.Errorf("= %q, %v", got, err)
		}
	})

	t.Run("empty stdin is an error, not an empty key", func(t *testing.T) {
		withStdin(t, "   \n")
		if _, err := readSecretT("-"); err == nil {
			t.Error("an empty pipe was accepted as a key")
		}
	})

	t.Run("nothing anywhere explains the options", func(t *testing.T) {
		t.Setenv("AGENTSWAP_API_KEY", "")
		withStdin(t, "") // a pipe, so there is nobody to prompt
		_, err := readSecretT("")
		if err == nil {
			t.Fatal("want an error")
		}
		for _, want := range []string{"--key -", "AGENTSWAP_API_KEY", "terminal"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})
}

// withStdin points os.Stdin at a pipe holding content, for the length of the
// test.
func withStdin(t *testing.T, content string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	go func() {
		_, _ = w.WriteString(content)
		_ = w.Close()
	}()

	saved := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = saved
		_ = r.Close()
	})
}

// /dev/null is a character device too, so a mode-bits check would call it a
// terminal and sit there waiting for a keystroke that cannot arrive.
func TestIsTerminalRejectsAPipe(t *testing.T) {
	withStdin(t, "")
	if isTerminal() {
		t.Error("a pipe was taken for a terminal")
	}

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip("no /dev/null here")
	}
	defer devNull.Close()
	saved := os.Stdin
	os.Stdin = devNull
	defer func() { os.Stdin = saved }()

	if isTerminal() {
		t.Error("/dev/null was taken for a terminal")
	}
}

// readSecretT keeps the existing cases readable now that the flag name is a
// parameter.
func readSecretT(v string) (string, error) { return readSecret(v, "--key") }
