package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func claudeSettings(e *env) string { return filepath.Join(e.claude, "settings.json") }
func codexConfig(e *env) string    { return filepath.Join(e.codex, "config.toml") }
func backupsIn(t *testing.T, dir, base string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), base+".agentswap-backup-") {
			out = append(out, en.Name())
		}
	}
	return out
}

// Losing a hand-tuned settings file to a tool that was supposed to help is not
// an acceptable outcome, so install must be reversible byte for byte.
func TestInstallAndUninstallRoundTrip(t *testing.T) {
	e := newEnv(t)

	original := `{
  "model": "opus",
  "env": {"MY_OWN_VAR": "keep-me"},
  "permissions": {"allow": ["Bash(ls)"]}
}
`
	writeFile(t, claudeSettings(e), original)
	codexOriginal := "model = \"gpt-5.6\"\n\n[profiles.mine]\nmodel_provider = \"openai\"\n"
	writeFile(t, codexConfig(e), codexOriginal)

	e.mustRun("install")

	settings := readFile(t, claudeSettings(e))
	for _, want := range []string{"ANTHROPIC_BASE_URL", "MY_OWN_VAR", "permissions", "opus"} {
		mustContain(t, settings, want, "settings after install")
	}
	codex := readFile(t, codexConfig(e))
	for _, want := range []string{"[model_providers.agentswap]", "wire_api", "profiles.mine", "gpt-5.6"} {
		mustContain(t, codex, want, "codex config after install")
	}

	// A backup before touching anything the user wrote.
	if got := backupsIn(t, e.claude, "settings.json"); len(got) == 0 {
		t.Error("no backup of settings.json was taken")
	}

	e.mustRun("uninstall")

	if got := readFile(t, claudeSettings(e)); !strings.Contains(got, "MY_OWN_VAR") ||
		strings.Contains(got, "ANTHROPIC_BASE_URL") {
		t.Errorf("settings not restored:\n%s", got)
	}
	if got := readFile(t, codexConfig(e)); got != codexOriginal {
		t.Errorf("codex config not restored byte for byte:\ngot:\n%s\nwant:\n%s", got, codexOriginal)
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	e := newEnv(t)
	e.mustRun("install")
	first := readFile(t, codexConfig(e))
	e.mustRun("install")
	second := readFile(t, codexConfig(e))

	if first != second {
		t.Errorf("a second install changed the file:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if n := strings.Count(second, "[model_providers.agentswap]"); n != 1 {
		t.Errorf("the agentswap block appears %d times, want 1", n)
	}
}

func TestInstallDryRunWritesNothing(t *testing.T) {
	e := newEnv(t)
	out := e.mustRun("install", "--dry-run").out()

	mustContain(t, out, "would create", "dry run")
	mustContain(t, out, "nothing was written", "dry run")
	if _, err := os.Stat(claudeSettings(e)); !os.IsNotExist(err) {
		t.Error("--dry-run wrote the settings file")
	}
	if _, err := os.Stat(codexConfig(e)); !os.IsNotExist(err) {
		t.Error("--dry-run wrote the codex config")
	}
}

func TestInstallOnlyOneCLI(t *testing.T) {
	e := newEnv(t)
	e.mustRun("install", "--only", "claude")

	if _, err := os.Stat(claudeSettings(e)); err != nil {
		t.Errorf("claude settings not written: %v", err)
	}
	if _, err := os.Stat(codexConfig(e)); !os.IsNotExist(err) {
		t.Error("--only claude touched the codex config")
	}
}

// A user who has since pointed the CLI at their own gateway keeps that setting.
func TestUninstallLeavesADeliberateOverrideAlone(t *testing.T) {
	e := newEnv(t)
	writeFile(t, claudeSettings(e), `{"env":{"ANTHROPIC_BASE_URL":"https://my-gateway.example"}}`)

	e.mustRun("uninstall")
	mustContain(t, readFile(t, claudeSettings(e)), "my-gateway.example", "uninstall")
}

func TestDoctorOnAWorkingSetup(t *testing.T) {
	e := newEnv(t)
	e.pinAddr()
	e.pool("only")
	e.serve()
	e.mustRun("install")

	r := e.run("doctor")
	if r.code != 0 {
		t.Fatalf("doctor exited %d on a healthy setup:\n%s", r.code, r.out())
	}
	mustContain(t, r.out(), "Everything checks out", "doctor")
}

// Most people use one of the two CLIs. Reporting the other as a fault makes the
// exit code meaningless.
func TestDoctorTreatsAnUnusedLaneAsANote(t *testing.T) {
	e := newEnv(t)
	e.pinAddr()
	e.pool("only") // anthropic only
	e.serve()
	e.mustRun("install", "--only", "claude")

	r := e.run("doctor")
	if r.code != 0 {
		t.Fatalf("doctor exited %d with only Claude Code set up:\n%s", r.code, r.out())
	}
	mustContain(t, r.out(), "openai lane has no accounts", "doctor")
	mustContain(t, r.out(), "fine if you do not use it", "doctor")
}

func TestDoctorReportsAMissingDaemon(t *testing.T) {
	e := newEnv(t)
	e.pool("only")

	r := e.run("doctor")
	if r.code == 0 {
		t.Error("doctor passed with no daemon running")
	}
	mustContain(t, r.out(), "agentswap serve", "doctor")
}

func TestDoctorWithAnEmptyPool(t *testing.T) {
	e := newEnv(t)
	r := e.run("doctor")
	if r.code == 0 {
		t.Error("doctor passed with an empty pool")
	}
	mustContain(t, r.out(), "agentswap import", "doctor")
}

// Being pointed at the wrong address is its own diagnosis: telling someone to
// run `install` when they already have would rewrite the same value.
func TestDoctorReportsAnAddressMismatch(t *testing.T) {
	e := newEnv(t)
	e.pool("only")

	// The CLIs are wired to the configured address, while the daemon was
	// started somewhere else — `serve --addr` is exactly this situation.
	e.pinAddr()
	e.mustRun("install")
	e.configAddr = ""
	d := e.serve()

	r := e.run("doctor")
	if r.code == 0 {
		t.Error("doctor passed with the CLIs pointed elsewhere")
	}
	mustContain(t, r.out(), "but the daemon is on "+d.addr, "doctor")
	mustNotContain(t, r.out(), "is pointed at agentswap\n", "doctor")
}
