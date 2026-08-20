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

func codexConfig(t *testing.T, content string) {
	t.Helper()
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		home = t.TempDir()
		t.Setenv("CODEX_HOME", home)
	}
	writeFile(t, filepath.Join(home, "config.toml"), content)
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

func TestImportClaudeAllIncludesNativeLoginAndProviderOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	credentials := filepath.Join(dir, ".credentials.json")
	writeFile(t, credentials, `{
	  "claudeAiOauth": {
	    "accessToken": "native-access",
	    "refreshToken": "native-refresh",
	    "subscriptionType": "max"
	  }
	}`)
	t.Setenv("CLAUDE_CREDENTIALS_PATH", credentials)
	writeFile(t, filepath.Join(dir, "settings.json"), `{
	  "env": {
	    "ANTHROPIC_BASE_URL": "https://api.krill-ai.net",
	    "ANTHROPIC_AUTH_TOKEN": "krill-token"
	  }
	}`)

	all, err := ImportClaudeAll()
	if err != nil {
		t.Fatalf("ImportClaudeAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("accounts = %d, want native login plus Krill override", len(all))
	}
	if all[0].Kind != store.KindOAuth || all[0].AccessToken != "native-access" {
		t.Errorf("native = %#v", all[0])
	}
	override := all[1]
	if override.Kind != store.KindAPIKey || override.APIKey != "krill-token" {
		t.Errorf("override credential = %#v", override)
	}
	if override.BaseURL != "https://api.krill-ai.net" {
		t.Errorf("override base_url = %q", override.BaseURL)
	}
	if override.AuthStyle != store.AuthStyleBearer {
		t.Errorf("override auth style = %q, want bearer for ANTHROPIC_AUTH_TOKEN", override.AuthStyle)
	}
}

func TestImportClaudeAllUsesXAPIKeyStyle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("CLAUDE_CREDENTIALS_PATH", filepath.Join(dir, "missing-credentials.json"))
	writeFile(t, filepath.Join(dir, "settings.json"), `{
	  "env": {
	    "ANTHROPIC_BASE_URL": "https://gateway.example",
	    "ANTHROPIC_API_KEY": "gateway-key"
	  }
	}`)

	all, err := ImportClaudeAll()
	if err != nil {
		t.Fatalf("ImportClaudeAll: %v", err)
	}
	if len(all) != 1 || all[0].AuthStyle != store.AuthStyleXAPIKey {
		t.Fatalf("accounts = %#v, want one x-api-key override", all)
	}
}

func TestImportClaudeAllDoesNotReimportAgentSwapPlaceholder(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("CLAUDE_CREDENTIALS_PATH", filepath.Join(dir, "missing-credentials.json"))
	writeFile(t, filepath.Join(dir, "settings.json"), `{
	  "env": {
	    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8420/anthropic",
	    "ANTHROPIC_AUTH_TOKEN": "agentswap-managed"
	  }
	}`)

	if _, err := ImportClaudeAll(); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("err = %v, want the managed placeholder ignored", err)
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

func TestImportCodexAllIncludesSubscriptionAndSelectedProvider(t *testing.T) {
	codexAt(t, `{
	  "OPENAI_API_KEY": "krill-key",
	  "tokens": {
	    "access_token": "chatgpt-access",
	    "refresh_token": "chatgpt-refresh",
	    "account_id": "acct-123"
	  }
	}`)
	codexConfig(t, `
model_provider = "krill"

[model_providers.krill]
base_url = "https://api.krill-ai.net/codex/v1"
name = "krill"
requires_openai_auth = true
wire_api = "responses"
`)

	all, err := ImportCodexAll()
	if err != nil {
		t.Fatalf("ImportCodexAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("accounts = %d, want ChatGPT login plus Krill provider", len(all))
	}
	if all[0].Kind != store.KindOAuth || all[0].ChatGPTAccountID != "acct-123" {
		t.Errorf("native subscription = %#v", all[0])
	}
	provider := all[1]
	if provider.Kind != store.KindAPIKey || provider.APIKey != "krill-key" {
		t.Errorf("provider credential = %#v", provider)
	}
	if provider.BaseURL != "https://api.krill-ai.net/codex/v1" {
		t.Errorf("provider base_url = %q", provider.BaseURL)
	}
}

func TestImportCodexRoutesSelectedProviderKeyToItsBaseURL(t *testing.T) {
	codexAt(t, `{"OPENAI_API_KEY":"krill-key"}`)
	codexConfig(t, `
model_provider = "krill" # preserve an inline comment

[model_providers.krill]
base_url = "https://api.krill-ai.net/codex/v1"
requires_openai_auth = true
`)

	a, err := ImportCodex()
	if err != nil {
		t.Fatalf("ImportCodex: %v", err)
	}
	if a.APIKey != "krill-key" || a.BaseURL != "https://api.krill-ai.net/codex/v1" {
		t.Errorf("account = %#v, want the key bound to Krill", a)
	}
}

func TestImportCodexProviderCanUseEnvKeyWithoutAuthFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("KRILL_API_KEY", "from-env")
	codexConfig(t, `
model_provider = 'krill'

[model_providers.'krill']
base_url = 'https://api.krill-ai.net/codex/v1'
env_key = 'KRILL_API_KEY'
`)

	all, err := ImportCodexAll()
	if err != nil {
		t.Fatalf("ImportCodexAll: %v", err)
	}
	if len(all) != 1 || all[0].APIKey != "from-env" || all[0].BaseURL == "" {
		t.Fatalf("accounts = %#v", all)
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
