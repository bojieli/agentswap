package e2e

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// claudeLogin writes what a `claude` login leaves behind.
func claudeLogin(t *testing.T, e *env, token string) {
	t.Helper()
	writeFile(t, filepath.Join(e.claude, ".credentials.json"), `{
	  "claudeAiOauth": {
	    "accessToken": "`+token+`",
	    "refreshToken": "`+token+`-refresh",
	    "expiresAt": 4102444800000,
	    "scopes": ["user:inference"],
	    "subscriptionType": "max"
	  }
	}`)
}

// codexLogin writes what a `codex login` leaves behind.
func codexLogin(t *testing.T, e *env, token string) {
	t.Helper()
	writeFile(t, filepath.Join(e.codex, "auth.json"), `{
	  "tokens": {
	    "access_token": "`+token+`",
	    "refresh_token": "`+token+`-refresh",
	    "account_id": "acct-`+token+`"
	  }
	}`)
}

func TestHelpAndVersion(t *testing.T) {
	e := newEnv(t)

	// No arguments is a usage error, not a crash and not a success.
	if r := e.run(); r.code != 2 {
		t.Errorf("bare invocation exited %d, want 2\n%s", r.code, r.out())
	}
	mustContain(t, e.run().out(), "Getting started", "usage")

	for _, flag := range []string{"-h", "--help", "help"} {
		r := e.run(flag)
		if r.code != 0 {
			t.Errorf("%s exited %d, want 0", flag, r.code)
		}
		mustContain(t, r.out(), "agentswap <command>", flag)
	}

	// An unknown command must not look like success to a script.
	r := e.run("frobnicate")
	if r.code != 2 {
		t.Errorf("unknown command exited %d, want 2", r.code)
	}
	mustContain(t, r.out(), "unknown command", "unknown command")

	mustContain(t, e.mustRun("version").out(), "agentswap", "version")
}

func TestImportBothLanes(t *testing.T) {
	e := newEnv(t)
	claudeLogin(t, e, "claude-token")
	codexLogin(t, e, "codex-token")

	out := e.mustRun("import").out()
	mustContain(t, out, "anthropic", "import")
	mustContain(t, out, "openai", "import")

	list := e.mustRun("list").out()
	mustContain(t, list, "anthropic-1", "list")
	mustContain(t, list, "openai-1", "list")

	// Credentials must never be echoed back at the terminal.
	mustNotContain(t, out+list, "claude-token", "import output")
}

func TestImportSkipsALaneWithNoLogin(t *testing.T) {
	e := newEnv(t)
	claudeLogin(t, e, "claude-token")

	out := e.mustRun("import").out()
	mustContain(t, out, "skipped: not logged in", "import")

	// A machine logged into one CLI is the normal case, not a failure.
	list := e.mustRun("list").out()
	if got := accountRows(list); got != 1 {
		t.Errorf("pooled %d accounts, want 1:\n%s", got, list)
	}
	mustNotContain(t, list, "openai", "list")
}

// The README documents this exact invocation for pooling a second account.
func TestImportWithAnIDNamesTheAccountVerbatim(t *testing.T) {
	e := newEnv(t)
	claudeLogin(t, e, "first-token")
	e.mustRun("import")

	// Log in as somebody else, then pool that too.
	claudeLogin(t, e, "second-token")
	out := e.mustRun("import", "--id", "work").out()
	mustContain(t, out, `"work"`, "import --id")

	list := e.mustRun("list").out()
	mustContain(t, list, "work", "list")
	// The first account must survive: pooling that overwrites is not pooling.
	mustContain(t, list, "anthropic-1", "list")

	accounts := readFile(t, filepath.Join(e.home, "accounts.json"))
	if !strings.Contains(accounts, "first-token") || !strings.Contains(accounts, "second-token") {
		t.Errorf("expected both credentials in the pool:\n%s", accounts)
	}
}

func TestImportAgainRefreshesInPlace(t *testing.T) {
	e := newEnv(t)
	claudeLogin(t, e, "stale-token")
	e.mustRun("import", "--id", "personal")

	claudeLogin(t, e, "renewed-token")
	out := e.mustRun("import", "--id", "personal").out()
	mustContain(t, out, "updated", "re-import")

	accounts := readFile(t, filepath.Join(e.home, "accounts.json"))
	if strings.Contains(accounts, "stale-token") {
		t.Error("re-importing the same id left the old credential behind")
	}
	if !strings.Contains(accounts, "renewed-token") {
		t.Error("re-importing did not store the new credential")
	}
}

func TestImportWithNothingToImport(t *testing.T) {
	e := newEnv(t)
	r := e.run("import")
	if r.code == 0 {
		t.Errorf("import with no logins succeeded, want a failure\n%s", r.out())
	}
	mustContain(t, r.out(), "log in", "import")
}

func TestAddKeyAndLifecycle(t *testing.T) {
	e := newEnv(t)

	// The lane is positional and must survive flag parsing.
	e.mustRun("add-key", "anthropic", "--key", "sk-ant-test", "--id", "mine")
	mustContain(t, e.mustRun("list").out(), "mine", "list")

	// A missing lane, an unknown lane and a missing key are all errors.
	for _, args := range [][]string{
		{"add-key"},
		{"add-key", "gemini", "--key", "x"},
		{"add-key", "anthropic"},
	} {
		if r := e.run(args...); r.code == 0 {
			t.Errorf("%v succeeded, want a failure\n%s", args, r.out())
		}
	}

	// Keeping a key out of shell history is the point of the env var.
	cmd := e.run("add-key", "openai", "--id", "from-env")
	if cmd.code == 0 {
		t.Error("add-key without a key succeeded")
	}

	if r := e.run("disable", "mine"); r.code != 0 {
		t.Fatalf("disable: %s", r.out())
	}
	mustContain(t, e.mustRun("list").out(), "disabled", "list after disable")
	e.mustRun("enable", "mine")
	mustNotContain(t, e.mustRun("list").out(), "disabled", "list after enable")

	e.mustRun("remove", "mine")
	mustContain(t, e.mustRun("list").out(), "no accounts yet", "list after remove")

	// Operating on an account that is not there is an error, not a no-op.
	for _, args := range [][]string{
		{"remove", "ghost"},
		{"enable", "ghost"},
		{"disable", "ghost"},
	} {
		if r := e.run(args...); r.code == 0 {
			t.Errorf("%v on a missing account succeeded, want a failure", args)
		}
	}
}

func TestAddKeyReadsTheEnvironment(t *testing.T) {
	e := newEnv(t)
	cmd := e.run("add-key", "anthropic", "--id", "env-key")
	if cmd.code == 0 {
		t.Fatal("add-key with no key anywhere succeeded")
	}

	// Same command, with the key supplied out of band.
	r := e.runEnv([]string{"AGENTSWAP_API_KEY=sk-ant-from-env"},
		"add-key", "anthropic", "--id", "env-key")
	if r.code != 0 {
		t.Fatalf("add-key via AGENTSWAP_API_KEY: %s", r.out())
	}
	mustContain(t, e.mustRun("list").out(), "env-key", "list")
}

func TestEnvPrintsUsableExports(t *testing.T) {
	e := newEnv(t)
	out := e.mustRun("env").out()

	for _, want := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "API_TIMEOUT_MS", "export "} {
		mustContain(t, out, want, "env")
	}
	// Codex cannot be configured through the environment, and saying so here
	// saves someone an hour.
	mustContain(t, out, "codex --profile", "env")
}

func TestStatusWithoutAccounts(t *testing.T) {
	e := newEnv(t)
	mustContain(t, e.mustRun("status").out(), "no accounts yet", "status")
}

func TestConfigErrorsAreReadable(t *testing.T) {
	e := newEnv(t)

	writeFile(t, filepath.Join(e.home, "config.json"), `{"addr": `)
	r := e.run("list")
	if r.code == 0 {
		t.Error("a truncated config was accepted")
	}
	mustContain(t, r.out(), "config.json", "config error")

	// A value that is valid JSON but impossible should also be caught before
	// anything starts behaving oddly under load.
	writeFile(t, filepath.Join(e.home, "config.json"), `{"rotation":{"drain_above":150}}`)
	r = e.run("list")
	if r.code == 0 {
		t.Error("an out-of-range drain_above was accepted")
	}
	mustContain(t, r.out(), "drain_above", "config error")
}

func TestCorruptAccountsFileIsReported(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.home, "accounts.json"), `[{"id":`)

	r := e.run("list")
	if r.code == 0 {
		t.Error("a corrupt pool was reported as an empty one")
	}
	mustContain(t, r.out(), "accounts.json", "corrupt pool")
}

func TestCredentialFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits do not apply")
	}
	e := newEnv(t)
	e.mustRun("add-key", "anthropic", "--key", "sk-ant-secret")

	info, err := os.Stat(filepath.Join(e.home, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("accounts.json mode = %o, want no group or world access", perm)
	}
}

// accountRows counts the data rows of a `list` table, ignoring its header.
func accountRows(listOutput string) int {
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(listOutput), "\n") {
		if line == "" || strings.HasPrefix(line, "ID ") {
			continue
		}
		n++
	}
	return n
}

// accounts.json is documented as hand-editable. A minimal entry someone typed
// has to work, rather than being silently inert because JSON's zero value for
// a bool is false.
func TestHandWrittenAccountWorks(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.home, "accounts.json"), `[
	  {"id": "mine", "lane": "anthropic", "kind": "api_key", "api_key": "sk-ant-hand-written",
	   "base_url": "`+e.upstream.url()+`"}
	]`)

	mustContain(t, e.mustRun("list").out(), "available", "list")

	d := e.serve()
	resp, body := d.post(t, "/anthropic/v1/messages", `{}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if got := e.upstream.keys(); len(got) != 1 || got[0] != "sk-ant-hand-written" {
		t.Errorf("upstream saw %v, want the hand-written account used", got)
	}
}

// Disabling must still be honoured — it is the deliberate act.
func TestExplicitlyDisabledStaysDisabled(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.home, "accounts.json"), `[
	  {"id": "off", "lane": "anthropic", "kind": "api_key", "api_key": "k", "enabled": false}
	]`)
	mustContain(t, e.mustRun("list").out(), "disabled", "list")

	// And the advice has to be about enabling it, not importing another.
	r := e.run("doctor")
	if r.code == 0 {
		t.Error("doctor passed with nothing usable")
	}
	mustContain(t, r.out(), "agentswap enable off", "doctor")
	mustNotContain(t, r.out(), "agentswap import", "doctor")
}
