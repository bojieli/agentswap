package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// claudeDir is the Claude Code config directory used by these tests. Setting
// CLAUDE_CONFIG_DIR rather than HOME keeps them hermetic on Windows too, where
// os.UserHomeDir reads %USERPROFILE% and a test that sets HOME would quietly
// rewrite the real one.
func claudeDir(base string) string { return filepath.Join(base, ".claude") }

const addr = "127.0.0.1:8420"

// testMaxHold is the default park.max_hold; the client timeout is derived
// from it.
const testMaxHold = 30 * time.Minute

func TestInstallClaudePreservesUnrelatedSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir(home))
	if err := os.MkdirAll(claudeDir(home), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(claudeDir(home), "settings.json")
	original := `{
  "model": "opus[1m]",
  "theme": "light",
  "env": {"MY_OWN_VAR": "keep-me"},
  "permissions": {"allow": ["Bash(ls)"]}
}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallClaude(addr, testMaxHold, false); err != nil {
		t.Fatalf("install: %v", err)
	}

	var got map[string]any
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("result is not valid json: %v", err)
	}

	if got["model"] != "opus[1m]" || got["theme"] != "light" {
		t.Errorf("unrelated top-level settings were lost: %v", got)
	}
	if _, ok := got["permissions"]; !ok {
		t.Error("permissions block was dropped")
	}
	env := got["env"].(map[string]any)
	if env["MY_OWN_VAR"] != "keep-me" {
		t.Error("the user's own env var was clobbered")
	}
	if env["ANTHROPIC_BASE_URL"] != "http://"+addr+"/anthropic" {
		t.Errorf("base url = %v", env["ANTHROPIC_BASE_URL"])
	}
	// Parking holds a request open with no bytes written, so the client's own
	// timeout has to be raised or it gives up mid-wait.
	if env["API_TIMEOUT_MS"] == nil {
		t.Error("API_TIMEOUT_MS was not set; parked requests would time out client-side")
	}
}

func TestUninstallClaudeRestoresTheUsersEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir(home))
	if err := os.MkdirAll(claudeDir(home), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(claudeDir(home), "settings.json")
	if err := os.WriteFile(path, []byte(`{"env":{"MY_OWN_VAR":"keep-me"},"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallClaude(addr, testMaxHold, false); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := UninstallClaude(addr); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	var got map[string]any
	b, _ := os.ReadFile(path)
	_ = json.Unmarshal(b, &got)

	env := got["env"].(map[string]any)
	if env["MY_OWN_VAR"] != "keep-me" {
		t.Error("uninstall removed the user's own env var")
	}
	for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "API_TIMEOUT_MS"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s survived uninstall", k)
		}
	}
	if got["theme"] != "dark" {
		t.Error("unrelated settings lost during uninstall")
	}
}

func TestUninstallClaudeLeavesADeliberateOverrideAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir(home))
	if err := os.MkdirAll(claudeDir(home), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(claudeDir(home), "settings.json")
	// The user has since pointed the CLI at their own gateway. That is not
	// ours to delete.
	if err := os.WriteFile(path, []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://my-gateway.example"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UninstallClaude(addr); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "my-gateway.example") {
		t.Errorf("uninstall deleted a setting it did not create: %s", b)
	}
}

func TestCodexBlockIsAdditiveAndRemovable(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	path := filepath.Join(codexHome, "config.toml")

	original := `model = "gpt-5.6-sol"
model_provider = "krill"

[model_providers.krill]
base_url = "https://api.krill-ai.net/codex/v1"
wire_api = "responses"
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallCodex(addr, false); err != nil {
		t.Fatalf("install: %v", err)
	}
	after, _ := os.ReadFile(path)
	s := string(after)
	profilePath := filepath.Join(codexHome, ProfileName+".config.toml")
	profile, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("read Codex profile: %v", err)
	}
	profileText := string(profile)

	// The user's existing provider and top-level keys must survive verbatim:
	// rewriting model_provider in place is exactly the kind of edit that
	// silently breaks someone's working setup.
	if !strings.Contains(s, `model_provider = "krill"`) {
		t.Error("existing model_provider was modified")
	}
	if !strings.Contains(s, "api.krill-ai.net") {
		t.Error("existing provider block was lost")
	}
	if strings.Contains(s, "[profiles.agentswap]") {
		t.Error("legacy agentswap profile table was added")
	}
	if !strings.Contains(profileText, "model_provider = \"agentswap\"") {
		t.Error("agentswap profile was not added")
	}
	if ok, err := CodexProfileUsesAgentSwap(); err != nil || !ok {
		t.Errorf("CodexProfileUsesAgentSwap = %v, %v; want true", ok, err)
	}

	if err := UninstallCodex(); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	restored, _ := os.ReadFile(path)
	if string(restored) != original {
		t.Errorf("uninstall did not restore the file exactly:\n--- got ---\n%s\n--- want ---\n%s", restored, original)
	}
	if _, err := os.Stat(profilePath); !os.IsNotExist(err) {
		t.Errorf("uninstall left the managed profile behind: %v", err)
	}
	if ok, err := CodexProfileUsesAgentSwap(); err != nil || ok {
		t.Errorf("CodexProfileUsesAgentSwap after uninstall = %v, %v; want false", ok, err)
	}
}

func TestInstallCodexIsIdempotent(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)

	for i := 0; i < 3; i++ {
		if _, err := InstallCodex(addr, false); err != nil {
			t.Fatalf("install %d: %v", i, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	profile, _ := os.ReadFile(filepath.Join(codexHome, ProfileName+".config.toml"))
	// A duplicated TOML table is a parse error, so repeated installs must
	// replace rather than accumulate.
	if n := strings.Count(string(b), "[model_providers.agentswap]"); n != 1 {
		t.Errorf("provider block appears %d times, want 1", n)
	}
	if n := strings.Count(string(profile), "model_provider = \"agentswap\""); n != 1 {
		t.Errorf("profile selector appears %d times, want 1", n)
	}
}

func TestInstallCodexMigratesItsLegacyProfileTable(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	path := filepath.Join(codexHome, "config.toml")
	legacy := `model = "gpt-5.6-sol"

# >>> agentswap >>>
# Added by an older agentswap release.
[model_providers.agentswap]
name = "agentswap"
base_url = "http://old.example/openai"
wire_api = "responses"
requires_openai_auth = true

[profiles.agentswap]
model_provider = "agentswap"
# <<< agentswap <<<
`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallCodex(addr, false); err != nil {
		t.Fatalf("migrate install: %v", err)
	}
	base, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(base), "[profiles.agentswap]") || strings.Contains(string(base), "old.example") {
		t.Errorf("legacy Codex profile survived migration:\n%s", base)
	}
	if !strings.Contains(string(base), "http://"+addr+"/openai") {
		t.Errorf("current provider address missing after migration:\n%s", base)
	}
	profile, err := os.ReadFile(filepath.Join(codexHome, ProfileName+".config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(profile), `model_provider = "agentswap"`) {
		t.Errorf("v2 profile missing after migration:\n%s", profile)
	}
}

func TestCodexProfilePreservesUserSettingsAndUninstallRestoresThem(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	profilePath := filepath.Join(codexHome, ProfileName+".config.toml")
	original := "model = \"gpt-5.6-sol\"\nmodel_reasoning_effort = \"high\"\n\n[features]\nweb_search = true\n"
	if err := os.WriteFile(profilePath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallCodex(addr, false); err != nil {
		t.Fatalf("install: %v", err)
	}
	installed, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{original, `model_provider = "agentswap"`} {
		if !strings.Contains(string(installed), want) {
			t.Errorf("installed profile omitted %q:\n%s", want, installed)
		}
	}
	if err := UninstallCodex(); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	restored, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Errorf("profile not restored exactly:\ngot:\n%s\nwant:\n%s", restored, original)
	}
}

func TestCodexUninstallPreservesAPreexistingEmptyProfileFile(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	profilePath := filepath.Join(codexHome, ProfileName+".config.toml")
	if err := os.WriteFile(profilePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallCodex(addr, false); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := UninstallCodex(); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	b, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("preexisting empty profile was removed: %v", err)
	}
	if len(b) != 0 {
		t.Errorf("preexisting empty profile gained content: %q", b)
	}
}

func TestInstallCodexRefusesUserOwnedAgentSwapProfileProvider(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	configPath := filepath.Join(codexHome, "config.toml")
	profilePath := filepath.Join(codexHome, ProfileName+".config.toml")
	configOriginal := "model = \"gpt-5.6-sol\"\n"
	profileOriginal := "model_provider = \"my-gateway\"\n"
	if err := os.WriteFile(configPath, []byte(configOriginal), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profilePath, []byte(profileOriginal), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallCodex(addr, false); err == nil || !strings.Contains(err.Error(), "already selects a Codex provider") {
		t.Fatalf("install conflict error = %v", err)
	}
	for path, want := range map[string]string{configPath: configOriginal, profilePath: profileOriginal} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("conflicting install changed %s:\ngot %q\nwant %q", path, got, want)
		}
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	codexHome := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir(home))
	t.Setenv("CODEX_HOME", codexHome)

	if _, err := InstallClaude(addr, testMaxHold, true); err != nil {
		t.Fatalf("claude dry run: %v", err)
	}
	if _, err := InstallCodex(addr, true); err != nil {
		t.Fatalf("codex dry run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(claudeDir(home), "settings.json")); !os.IsNotExist(err) {
		t.Error("dry run created the claude settings file")
	}
	if _, err := os.Stat(filepath.Join(codexHome, "config.toml")); !os.IsNotExist(err) {
		t.Error("dry run created the codex config file")
	}
	if _, err := os.Stat(filepath.Join(codexHome, ProfileName+".config.toml")); !os.IsNotExist(err) {
		t.Error("dry run created the codex profile file")
	}
}

func TestInstallBacksUpBeforeModifying(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir(home))
	if err := os.MkdirAll(claudeDir(home), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(claudeDir(home), "settings.json")
	if err := os.WriteFile(path, []byte(`{"theme":"light"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallClaude(addr, testMaxHold, false); err != nil {
		t.Fatalf("install: %v", err)
	}
	matches, _ := filepath.Glob(path + ".agentswap-backup-*")
	if len(matches) != 1 {
		t.Fatalf("got %d backups, want 1", len(matches))
	}
	b, _ := os.ReadFile(matches[0])
	if string(b) != `{"theme":"light"}` {
		t.Errorf("backup content = %q", b)
	}
}

// The client's own timeout has to outlast the daemon's longest park. If it
// does not, a wait that was about to succeed is cut off by the CLI instead —
// and the daemon never learns why.
func TestClientTimeoutOutlastsTheLongestPark(t *testing.T) {
	cases := []time.Duration{
		30 * time.Minute, // the default
		2 * time.Hour,
		5 * time.Hour, // a full Anthropic window
	}
	for _, maxHold := range cases {
		env := ClaudeEnv(addr, maxHold)
		ms, err := strconv.ParseInt(env["API_TIMEOUT_MS"], 10, 64)
		if err != nil {
			t.Fatalf("API_TIMEOUT_MS = %q: %v", env["API_TIMEOUT_MS"], err)
		}
		got := time.Duration(ms) * time.Millisecond
		if got <= maxHold {
			t.Errorf("max_hold %v: client timeout %v does not outlast it", maxHold, got)
		}
	}
}

// park.max_hold can change between install and uninstall, which changes the
// derived timeout. Uninstall still has to recognise the block as its own.
func TestUninstallRemovesTheBlockAfterMaxHoldChanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir(home))
	if err := os.MkdirAll(claudeDir(home), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallClaude(addr, 30*time.Minute, false); err != nil {
		t.Fatalf("install: %v", err)
	}
	// The user raised max_hold, then uninstalled.
	if _, err := InstallClaude(addr, 4*time.Hour, false); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if err := UninstallClaude(addr); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	b, _ := os.ReadFile(filepath.Join(claudeDir(home), "settings.json"))
	for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "API_TIMEOUT_MS"} {
		if strings.Contains(string(b), k) {
			t.Errorf("%s survived uninstall: %s", k, b)
		}
	}
}

// Claude Code honours CLAUDE_CONFIG_DIR, so agentswap has to as well. Writing
// to ~/.claude while the CLI reads elsewhere would leave `install` reporting
// success having configured nothing — and `doctor` insisting the CLI is not
// wired up however many times you run it.
func TestInstallFollowsClaudeConfigDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "custom-claude")
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	plan, err := InstallClaude(addr, testMaxHold, false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	want := filepath.Join(dir, "settings.json")
	if plan.Path != want {
		t.Errorf("wrote %q, want %q", plan.Path, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("settings not written to the configured directory: %v", err)
	}
}
