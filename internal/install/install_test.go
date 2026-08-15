package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const addr = "127.0.0.1:8420"

func TestInstallClaudePreservesUnrelatedSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude", "settings.json")
	original := `{
  "model": "opus[1m]",
  "theme": "light",
  "env": {"MY_OWN_VAR": "keep-me"},
  "permissions": {"allow": ["Bash(ls)"]}
}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallClaude(addr, false); err != nil {
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
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.WriteFile(path, []byte(`{"env":{"MY_OWN_VAR":"keep-me"},"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallClaude(addr, false); err != nil {
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
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude", "settings.json")
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

	// The user's existing provider and top-level keys must survive verbatim:
	// rewriting model_provider in place is exactly the kind of edit that
	// silently breaks someone's working setup.
	if !strings.Contains(s, `model_provider = "krill"`) {
		t.Error("existing model_provider was modified")
	}
	if !strings.Contains(s, "api.krill-ai.net") {
		t.Error("existing provider block was lost")
	}
	if !strings.Contains(s, "[profiles.agentswap]") {
		t.Error("agentswap profile was not added")
	}

	if err := UninstallCodex(); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	restored, _ := os.ReadFile(path)
	if string(restored) != original {
		t.Errorf("uninstall did not restore the file exactly:\n--- got ---\n%s\n--- want ---\n%s", restored, original)
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
	// A duplicated TOML table is a parse error, so repeated installs must
	// replace rather than accumulate.
	if n := strings.Count(string(b), "[profiles.agentswap]"); n != 1 {
		t.Errorf("profile block appears %d times, want 1", n)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	codexHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)

	if _, err := InstallClaude(addr, true); err != nil {
		t.Fatalf("claude dry run: %v", err)
	}
	if _, err := InstallCodex(addr, true); err != nil {
		t.Fatalf("codex dry run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Error("dry run created the claude settings file")
	}
	if _, err := os.Stat(filepath.Join(codexHome, "config.toml")); !os.IsNotExist(err) {
		t.Error("dry run created the codex config file")
	}
}

func TestInstallBacksUpBeforeModifying(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.WriteFile(path, []byte(`{"theme":"light"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallClaude(addr, false); err != nil {
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
