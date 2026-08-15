package e2e

import (
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Pooling one login twice is worse than pooling it once: `status` shows two
// accounts, the user believes they have failover, and both are refused in the
// same instant.
func TestImportingTheSameLoginTwiceDoesNotDuplicate(t *testing.T) {
	e := newEnv(t)
	claudeLogin(t, e, "one-and-only")

	e.mustRun("import")
	second := e.mustRun("import")

	mustContain(t, second.out(), "already in the pool", "second import")
	if got := accountRows(e.mustRun("list").out()); got != 1 {
		t.Errorf("pool holds %d accounts, want 1:\n%s", got, e.mustRun("list").out())
	}
}

// A refresh rotates the access token constantly, so identity cannot rest on it.
func TestImportRecognisesARefreshedToken(t *testing.T) {
	e := newEnv(t)
	claudeLogin(t, e, "original")
	e.mustRun("import", "--id", "personal")

	// The CLI refreshed: new access token, same refresh token.
	writeFile(t, filepath.Join(e.claude, ".credentials.json"), `{
	  "claudeAiOauth": {
	    "accessToken": "rotated-access",
	    "refreshToken": "original-refresh",
	    "expiresAt": 4102444800000
	  }
	}`)
	out := e.mustRun("import").out()

	mustContain(t, out, "already in the pool", "import after a refresh")
	if got := accountRows(e.mustRun("list").out()); got != 1 {
		t.Errorf("pool holds %d accounts, want 1", got)
	}
}

func TestAddingTheSameKeyTwiceDoesNotDuplicate(t *testing.T) {
	e := newEnv(t)
	e.mustRun("add-key", "anthropic", "--key", "sk-ant-the-same")
	second := e.mustRun("add-key", "anthropic", "--key", "sk-ant-the-same")

	mustContain(t, second.out(), "already in the pool", "second add-key")
	if got := accountRows(e.mustRun("list").out()); got != 1 {
		t.Errorf("pool holds %d keys, want 1", got)
	}
}

// A key on the command line lands in shell history and the process list.
func TestAddKeyReadsAPipe(t *testing.T) {
	e := newEnv(t)

	cmd := exec.Command(binary, "add-key", "anthropic", "--key", "-", "--id", "piped")
	cmd.Env = e.environ()
	cmd.Stdin = strings.NewReader("sk-ant-api03-frompipe1234\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("add-key --key -: %v\n%s", err, out)
	}

	list := e.mustRun("list").out()
	mustContain(t, list, "piped", "list")
	// Masked, so several keys can be told apart without any of them on screen.
	mustContain(t, list, "1234", "list")
	mustNotContain(t, list, "frompipe", "list")
}

// Someone with an official key, a gateway key and a subscription has to be able
// to see which row is which.
func TestListDistinguishesCredentials(t *testing.T) {
	e := newEnv(t)
	e.mustRun("add-key", "anthropic", "--key", "sk-ant-api03-officialkey99", "--id", "official")
	e.mustRun("add-key", "anthropic", "--key", "gw-key-1234", "--id", "gateway",
		"--base-url", "https://llm.corp.example.com/v1")
	claudeLogin(t, e, "subscription-token")
	e.mustRun("import", "--id", "personal")

	list := e.mustRun("list").out()
	mustContain(t, list, "llm.corp.example.com", "list")
	mustContain(t, list, "max", "list") // the plan, from the login fixture
	mustNotContain(t, list, "officialkey99", "list")
	mustNotContain(t, list, "subscription-token", "list")
}

// The error the agent shows is the only channel most people will ever read.
func TestRejectedCredentialsTellTheUserWhatToRun(t *testing.T) {
	e := newEnv(t)
	e.pool("first", "second")
	e.upstream.handle(func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"authentication_error"}}`)
	})
	d := e.serve()

	resp, body := d.post(t, "/anthropic/v1/messages", `{}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{"rejected", "first", "second", "agentswap login --id"} {
		mustContain(t, body, want, "rejected-pool error")
	}
	// Re-importing re-reads the credential that was just refused.
	mustNotContain(t, body, "agentswap import", "rejected-pool error")

	// And the pull channel says the same thing.
	status := e.mustRun("status").out()
	mustContain(t, status, "was rejected", "status")
	mustContain(t, status, "agentswap login --id first", "status")
}

// Signing in before reaching for agentswap is the common order, and there is
// nothing to wait for in that case.
func TestLoginAdoptsAnAlreadyFreshSignIn(t *testing.T) {
	e := newEnv(t)
	claudeLogin(t, e, "brand-new-account")

	out := e.mustRun("login", "--lane", "anthropic", "--id", "personal").out()

	mustContain(t, out, "added", "login")
	mustContain(t, out, "personal", "login")
	mustContain(t, e.mustRun("list").out(), "personal", "list")
	// One account is not failover, and saying so is the whole point of pooling.
	mustContain(t, out, "not failover", "login")
}

// The recovery loop the rejected-credentials error points at.
func TestLoginReplacesARejectedCredential(t *testing.T) {
	e := newEnv(t)
	claudeLogin(t, e, "dead-token")
	e.mustRun("import", "--id", "work")

	// The daemon marked it invalid after the upstream refused it.
	writeFile(t, filepath.Join(e.home, "state.json"),
		`{"work":{"state":"invalid","last_error":"refresh failed with 401 Unauthorized"}}`)
	mustContain(t, e.mustRun("status").out(), "agentswap login --id work", "status")

	// The user signs in again, then runs exactly what they were told.
	claudeLogin(t, e, "fresh-token")
	out := e.mustRun("login", "--id", "work").out()
	mustContain(t, out, "replaced the credential", "login")

	// Back in rotation: a credential that is still marked invalid is never tried.
	status := e.mustRun("status").out()
	mustNotContain(t, status, "invalid", "status after re-login")
	mustNotContain(t, status, "was rejected", "status after re-login")
	if got := accountRows(e.mustRun("list").out()); got != 1 {
		t.Errorf("pool holds %d accounts, want the one replaced in place", got)
	}
}

// Waiting is the point: the user signs in somewhere else and this notices.
func TestLoginWaitsForANewSignIn(t *testing.T) {
	e := newEnv(t)
	claudeLogin(t, e, "already-pooled")
	e.mustRun("import", "--id", "personal")

	cmd := exec.Command(binary, "login", "--lane", "anthropic", "--id", "work", "--timeout", "30s")
	cmd.Env = e.environ()
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Sign in as somebody else while it waits.
	time.Sleep(1500 * time.Millisecond)
	claudeLogin(t, e, "the-second-account")

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("login exited %v:\n%s", err, out.String())
		}
	case <-time.After(40 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("login never noticed the new sign-in:\n%s", out.String())
	}

	mustContain(t, out.String(), "added", "login")
	if got := accountRows(e.mustRun("list").out()); got != 2 {
		t.Errorf("pool holds %d accounts, want 2:\n%s", got, e.mustRun("list").out())
	}
}

func TestLoginWithoutACLISaysSo(t *testing.T) {
	e := newEnv(t)
	// A lane whose CLI is not installed and which has no credential.
	r := e.run("login", "--lane", "openai", "--timeout", "2s")
	if r.code == 0 {
		t.Error("login succeeded with nothing to sign in to")
	}
	mustContain(t, r.out(), "codex login", "login")
}

// An API key sitting in the environment is a fallback the user probably meant
// to have. Adopting it silently would be a surprise; not mentioning it is how
// people discover the fallback was never there.
func TestImportMentionsKeysInTheEnvironment(t *testing.T) {
	e := newEnv(t)
	claudeLogin(t, e, "tok")

	r := e.runEnv([]string{"ANTHROPIC_API_KEY=sk-ant-sitting-in-my-shell"}, "import")
	if r.code != 0 {
		t.Fatalf("import: %s", r.out())
	}
	mustContain(t, r.out(), "ANTHROPIC_API_KEY", "import")
	mustContain(t, r.out(), "add-key anthropic --key -", "import")
	// Mentioning it must not mean printing it.
	mustNotContain(t, r.out(), "sitting-in-my-shell", "import")

	// And it must not have been adopted behind the user's back.
	if got := accountRows(e.mustRun("list").out()); got != 1 {
		t.Errorf("pool holds %d accounts, want only the imported login", got)
	}
}

// Having both CLIs installed is normal and says nothing about which one you
// just signed in to. A credential sitting there unpooled does.
func TestLoginPicksTheLaneWithTheNewSignIn(t *testing.T) {
	e := newEnv(t)
	// Both CLIs are signed in, and the Codex one is already pooled.
	codexLogin(t, e, "codex-token")
	e.mustRun("import", "--id", "chatgpt")
	claudeLogin(t, e, "a-new-claude-account")

	out := e.mustRun("login", "--id", "personal").out()

	mustContain(t, out, "added", "login")
	list := e.mustRun("list").out()
	mustContain(t, list, "personal", "list")
	for _, line := range strings.Split(list, "\n") {
		if strings.HasPrefix(line, "personal") && !strings.Contains(line, "anthropic") {
			t.Errorf("personal landed in the wrong lane: %q", line)
		}
	}
}

// When it truly cannot tell, it has to say what to type rather than guess.
func TestLoginAsksWhenBothLanesAreSettled(t *testing.T) {
	e := newEnv(t)
	claudeLogin(t, e, "claude-token")
	codexLogin(t, e, "codex-token")
	e.mustRun("import") // both pooled, so neither has a new sign-in

	r := e.run("login", "--timeout", "2s")
	if r.code == 0 {
		t.Error("login guessed a lane when both were equally plausible")
	}
	mustContain(t, r.out(), "--lane anthropic", "login")
}

// Changing where an account points, or how it is ordered, must not require
// re-supplying its credential or opening the JSON by hand.
func TestSetChangesAnAccountInPlace(t *testing.T) {
	e := newEnv(t)
	e.mustRun("add-key", "anthropic", "--key", "sk-ant-secret-1234", "--id", "gw")

	e.mustRun("set", "gw", "--base-url", "https://llm.corp.example.com/v1")
	e.mustRun("set", "gw", "--priority", "200", "--label", "Corp gateway")

	list := e.mustRun("list").out()
	mustContain(t, list, "llm.corp.example.com", "list")
	mustContain(t, list, "200", "list")
	// The credential is untouched by all of that.
	mustContain(t, list, "1234", "list")

	// And it can be pointed back at the vendor's own API.
	e.mustRun("set", "gw", "--base-url", "")
	mustNotContain(t, e.mustRun("list").out(), "llm.corp.example.com", "list")
}

// A subscription can sit behind a gateway too, and that is not something
// add-key can express.
func TestSetWorksOnSubscriptions(t *testing.T) {
	e := newEnv(t)
	claudeLogin(t, e, "tok")
	e.mustRun("import", "--id", "personal")

	e.mustRun("set", "personal", "--base-url", "https://gateway.example.com", "--priority", "3")
	list := e.mustRun("list").out()
	mustContain(t, list, "gateway.example.com", "list")

	// Its credential comes from signing in, not from a flag.
	r := e.run("set", "personal", "--key", "-")
	if r.code == 0 {
		t.Error("set --key succeeded on a subscription")
	}
	mustContain(t, r.out(), "agentswap login --id personal", "set --key on a subscription")
}

func TestSetValidates(t *testing.T) {
	e := newEnv(t)
	e.mustRun("add-key", "anthropic", "--key", "k", "--id", "gw")

	for _, args := range [][]string{
		{"set", "gw", "--base-url", "notaurl"},
		{"set", "gw", "--base-url", "ftp://nope.example"},
		{"set", "missing", "--priority", "1"},
		{"set", "gw"}, // nothing to change
		{"set"},       // no id
	} {
		if r := e.run(args...); r.code == 0 {
			t.Errorf("%v succeeded, want a failure\n%s", args, r.out())
		}
	}
}

// "Where does this live and what is it doing" is one question, and every
// setting having a default makes an absent file a bad answer to it.
func TestConfigShowsWhereEverythingLives(t *testing.T) {
	e := newEnv(t)
	e.mustRun("add-key", "anthropic", "--key", "k")

	out := e.mustRun("config").out()
	for _, want := range []string{
		filepath.Join(e.home, "config.json"),
		filepath.Join(e.home, "accounts.json"),
		"AGENTSWAP_HOME", // why the directory is where it is
		"drain_above",    // the effective settings, not just the file
		"not running",    // the daemon
	} {
		mustContain(t, out, want, "config")
	}
	// It reports on credentials without printing any.
	mustNotContain(t, out, "\"api_key\"", "config")
}

// The obvious way to save the settings — redirecting --json into the file —
// truncates it before agentswap reads it, so --write exists instead.
func TestConfigWriteRoundTrips(t *testing.T) {
	e := newEnv(t)
	e.mustRun("config", "--write")

	path := filepath.Join(e.home, "config.json")
	mustContain(t, readFile(t, path), "max_hold", "written config")

	// Editing it takes effect, which is the whole point of writing it out.
	writeFile(t, path, strings.Replace(readFile(t, path), `"30m0s"`, `"4h0m0s"`, 1))
	mustContain(t, e.mustRun("config", "--json").out(), "4h0m0s", "config --json")

	// It will not silently replace settings that are already there.
	if r := e.run("config", "--write"); r.code == 0 {
		t.Error("--write overwrote an existing config without --force")
	}
	e.mustRun("config", "--write", "--force")
}

// An empty config.json is what a redirect leaves behind, and refusing to start
// over zero bytes would strand every command until the user deleted the file.
func TestEmptyConfigFileIsTreatedAsAbsent(t *testing.T) {
	e := newEnv(t)
	e.mustRun("add-key", "anthropic", "--key", "k")
	writeFile(t, filepath.Join(e.home, "config.json"), "")

	for _, cmd := range []string{"list", "status", "config"} {
		if r := e.run(cmd); r.code != 0 {
			t.Errorf("%s failed with an empty config.json:\n%s", cmd, r.out())
		}
	}
}
