package importer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bojieli/agentswap/internal/store"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// claudeAt points the importer at a credentials file under a temp dir.
func claudeAt(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".credentials.json")
	writeFile(t, path, content)
	t.Setenv("CLAUDE_CREDENTIALS_PATH", path)
}

// codexAt points the importer at an auth.json under a temp CODEX_HOME.
func codexAt(t *testing.T, content string) {
	t.Helper()
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "auth.json"), content)
	t.Setenv("CODEX_HOME", home)
}

func TestImportClaude(t *testing.T) {
	claudeAt(t, `{
	  "claudeAiOauth": {
	    "accessToken": "sk-ant-oat-access",
	    "refreshToken": "sk-ant-ort-refresh",
	    "expiresAt": 1789000000000,
	    "scopes": ["user:inference", "user:profile"],
	    "subscriptionType": "max"
	  },
	  "mcpOAuth": {"some-server": {"accessToken": "unrelated"}}
	}`)

	a, err := ImportClaude()
	if err != nil {
		t.Fatalf("ImportClaude: %v", err)
	}
	if a.Lane != store.LaneAnthropic || a.Kind != store.KindOAuth {
		t.Errorf("lane/kind = %s/%s, want anthropic/oauth", a.Lane, a.Kind)
	}
	if a.AccessToken != "sk-ant-oat-access" || a.RefreshToken != "sk-ant-ort-refresh" {
		t.Errorf("tokens = %q/%q", a.AccessToken, a.RefreshToken)
	}
	if a.ExpiresAt != 1789000000000 {
		t.Errorf("expires_at = %d, want the milliseconds from the file verbatim", a.ExpiresAt)
	}
	if a.SubscriptionType != "max" {
		t.Errorf("subscription = %q, want max", a.SubscriptionType)
	}
	if !a.Enabled {
		t.Error("imported account is disabled")
	}
	// An imported account is named by the caller, which is the only place that
	// knows how many credentials the import turned up.
	if a.ID != "" || a.Label != "" {
		t.Errorf("id/label = %q/%q, want them left to the caller", a.ID, a.Label)
	}
}

func TestImportClaudeWithoutATokenIsNotACredential(t *testing.T) {
	// The file exists — the user has run Claude Code — but they logged out, or
	// only ever authenticated an MCP server.
	claudeAt(t, `{"mcpOAuth": {"some-server": {"accessToken": "unrelated"}}}`)

	if _, err := ImportClaude(); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("err = %v, want ErrNoCredentials so import can report it as a skip", err)
	}
}

// A named path means that path. On macOS the Keychain fallback would otherwise
// import whichever account happens to be logged in there, which is a different
// credential than the one the user asked for — and pooling the wrong account is
// worse than reporting that the file is missing.
func TestImportClaudeDoesNotFallBackFromAnExplicitPath(t *testing.T) {
	t.Setenv("CLAUDE_CREDENTIALS_PATH", filepath.Join(t.TempDir(), "absent.json"))

	if _, err := ImportClaude(); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("err = %v, want ErrNoCredentials", err)
	}
}

func TestImportClaudeCorruptFileIsNotASkip(t *testing.T) {
	// A truncated credentials file is a real problem worth reporting, not a
	// "you are not logged in" that import would silently pass over.
	claudeAt(t, `{"claudeAiOauth": {`)

	_, err := ImportClaude()
	if err == nil {
		t.Fatal("want an error")
	}
	if errors.Is(err, ErrNoCredentials) {
		t.Errorf("err = %v, want a parse error rather than ErrNoCredentials", err)
	}
}

func TestImportCodexPrefersTheSubscription(t *testing.T) {
	// Both present: the subscription is already paid for, so it goes first.
	codexAt(t, `{
	  "OPENAI_API_KEY": "sk-proj-metered",
	  "tokens": {
	    "access_token": "chatgpt-access",
	    "refresh_token": "chatgpt-refresh",
	    "account_id": "acct-123"
	  }
	}`)

	a, err := ImportCodex()
	if err != nil {
		t.Fatalf("ImportCodex: %v", err)
	}
	if a.Kind != store.KindOAuth {
		t.Errorf("kind = %s, want oauth", a.Kind)
	}
	if a.ChatGPTAccountID != "acct-123" {
		t.Errorf("chatgpt account id = %q, want it carried over: without it the backend 401s on a valid token", a.ChatGPTAccountID)
	}
	if a.APIKey != "" {
		t.Errorf("api key = %q, want the subscription to win outright", a.APIKey)
	}
}

func TestImportCodexFallsBackToTheAPIKey(t *testing.T) {
	codexAt(t, `{"OPENAI_API_KEY": "sk-proj-metered"}`)

	a, err := ImportCodex()
	if err != nil {
		t.Fatalf("ImportCodex: %v", err)
	}
	if a.Kind != store.KindAPIKey || a.APIKey != "sk-proj-metered" {
		t.Errorf("kind/key = %s/%q, want api_key/sk-proj-metered", a.Kind, a.APIKey)
	}
}

func TestImportCodexEmptyAuth(t *testing.T) {
	codexAt(t, `{}`)

	if _, err := ImportCodex(); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("err = %v, want ErrNoCredentials", err)
	}
}

func TestImportCodexMissingFile(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "absent"))

	if _, err := ImportCodex(); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("err = %v, want ErrNoCredentials", err)
	}
}

func TestPathsHonorEnvironmentOverrides(t *testing.T) {
	t.Setenv("CODEX_HOME", "/tmp/codex-home")
	got, err := CodexAuthPath()
	if err != nil {
		t.Fatalf("CodexAuthPath: %v", err)
	}
	if want := filepath.Join("/tmp/codex-home", "auth.json"); got != want {
		t.Errorf("CodexAuthPath = %q, want %q", got, want)
	}

	t.Setenv("CLAUDE_CREDENTIALS_PATH", "/tmp/creds.json")
	if got, err := ClaudeCredentialPath(); err != nil || got != "/tmp/creds.json" {
		t.Errorf("ClaudeCredentialPath = %q, %v", got, err)
	}
}
