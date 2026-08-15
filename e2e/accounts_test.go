package e2e

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
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
	// The remedy has to fit the credential: these are keys, and telling
	// somebody to sign in to an API key wastes the one message they will read.
	for _, want := range []string{"rejected", "first", "second", "agentswap set first --key -"} {
		mustContain(t, body, want, "rejected-pool error")
	}
	// Re-importing re-reads the credential that was just refused.
	mustNotContain(t, body, "agentswap import", "rejected-pool error")

	// And the pull channel says the same thing.
	status := e.mustRun("status").out()
	mustContain(t, status, "was rejected", "status")
	mustContain(t, status, "agentswap set first --key -", "status")
}

// A subscription is signed into again; only a key is replaced.
func TestARejectedSubscriptionSaysToSignIn(t *testing.T) {
	e := newEnv(t)
	// No refresh attempt: this is about the message, and renewing a fabricated
	// token would mean a real call to the vendor's token endpoint.
	writeFile(t, filepath.Join(e.home, "config.json"),
		`{"retry":{"auth_refresh_attempts":0}}`)
	claudeLogin(t, e, "stale-token")
	e.mustRun("import", "--id", "personal")
	// Point it at the fake upstream, which refuses it.
	e.mustRun("set", "personal", "--base-url", e.upstream.url())
	e.upstream.handle(func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"token revoked"}}`)
	})
	d := e.serve()

	_, body := d.post(t, "/anthropic/v1/messages", `{}`)
	mustContain(t, body, "token revoked", "the refusal")
	mustContain(t, body, "agentswap login --id personal", "the refusal")
	mustNotContain(t, body, "--key -", "the refusal")
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

// A hand-edited file can say two things at once. An id is how everything
// addresses an account, so two of them silently halve the pool.
func TestDuplicateIDsAreRefused(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.home, "accounts.json"), `[
	  {"id":"same","lane":"anthropic","kind":"api_key","api_key":"a"},
	  {"id":"same","lane":"anthropic","kind":"api_key","api_key":"b"}
	]`)

	r := e.run("list")
	if r.code == 0 {
		t.Error("two accounts with one id were accepted")
	}
	mustContain(t, r.out(), "two accounts with the id", "list")
}

// One unusable account must not take away the commands needed to fix it.
func TestAnUnusableAccountIsReportedNotFatal(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.home, "accounts.json"), `[
	  {"id":"good","lane":"anthropic","kind":"api_key","api_key":"fine","base_url":"`+e.upstream.url()+`"},
	  {"id":"broken","lane":"anthropic","kind":"api_key"}
	]`)

	list := e.mustRun("list").out()
	mustContain(t, list, "unusable", "list")
	mustContain(t, list, "no api_key", "list")

	r := e.run("doctor")
	if r.code == 0 {
		t.Error("doctor passed with an unusable account")
	}
	mustContain(t, r.out(), "agentswap remove broken", "doctor")

	// The good one still serves, and the broken one is never tried.
	d := e.serve()
	resp, _ := d.post(t, "/anthropic/v1/messages", `{}`)
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want the usable account to serve", resp.StatusCode)
	}
	if got := e.upstream.keys(); len(got) != 1 || got[0] != "fine" {
		t.Errorf("upstream saw %v, want only the usable account", got)
	}
}

// Following one would send the pool's credential to a host nobody configured,
// and rewrite POST as GET on the way.
func TestRedirectsAreNotFollowed(t *testing.T) {
	e := newEnv(t)
	var elsewhereSaw string
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereSaw = r.Header.Get("X-Api-Key")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer elsewhere.Close()

	e.pool("secret-key")
	e.upstream.handle(func(w http.ResponseWriter, r *http.Request, _ recorded) {
		http.Redirect(w, r, elsewhere.URL+"/v1/messages", http.StatusFound)
	})
	d := e.serve()

	req, err := http.NewRequest(http.MethodPost,
		"http://"+d.addr+"/anthropic/v1/messages", strings.NewReader(`{"model":"claude"}`))
	if err != nil {
		t.Fatal(err)
	}
	// This client must not follow it either, or the test measures net/http
	// rather than the proxy.
	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if elsewhereSaw != "" {
		t.Errorf("the redirect target received our credential: %q", elsewhereSaw)
	}
	// The client gets the redirect and decides for itself.
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want the 302 handed back", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc == "" {
		t.Error("Location was not relayed, so the client cannot act on it")
	}
}

// The daemon has to keep running without a terminal held open for it.
func TestServiceStatusAndDryRun(t *testing.T) {
	e := newEnv(t)

	status := e.run("service", "status")
	if runtime.GOOS == "windows" {
		if status.code == 0 {
			t.Error("service status succeeded on a platform with no manager")
		}
		mustContain(t, status.out(), "Task Scheduler", "service status")
		return
	}
	if status.code != 0 {
		t.Fatalf("service status: %s", status.out())
	}
	mustContain(t, status.out(), "not installed", "service status")

	// --dry-run has to show the file without touching the system.
	dry := e.mustRun("service", "install", "--dry-run").out()
	mustContain(t, dry, "agentswap", "service install --dry-run")
	mustContain(t, dry, e.home, "service install --dry-run")
	if runtime.GOOS == "darwin" {
		mustContain(t, dry, "RunAtLoad", "the plist")
	} else {
		mustContain(t, dry, "ExecStart=", "the unit")
	}

	// And it must not have installed anything.
	mustContain(t, e.mustRun("service", "status").out(), "not installed", "service status")
}

// The daemon reads the pool at startup, so a credential replaced afterwards
// was invisible to it: the fix that `status` and the client's own error both
// recommend appeared to do nothing until somebody restarted the daemon.
func TestFixingAnAccountTakesEffectWithoutARestart(t *testing.T) {
	e := newEnv(t)
	e.mustRun("add-key", "anthropic", "--key", "wrong-key", "--id", "gw",
		"--base-url", e.upstream.url())

	refuse := true
	e.upstream.handle(func(w http.ResponseWriter, _ *http.Request, req recorded) {
		if refuse && req.Key == "wrong-key" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"message":"that key is not valid here"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	d := e.serve()

	resp, body := d.post(t, "/anthropic/v1/messages", `{}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the refusal surfaced\n%s", resp.StatusCode, body)
	}
	// The upstream's words, and a remedy that fits an API key.
	mustContain(t, body, "that key is not valid here", "the refusal")
	mustContain(t, body, "agentswap set gw --key -", "the refusal")

	// Do exactly what it said, with the daemon still running.
	refuse = false
	cmd := exec.Command(binary, "set", "gw", "--key", "-")
	cmd.Env = e.environ()
	cmd.Stdin = strings.NewReader("right-key\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("set: %v\n%s", err, out)
	}

	resp, body = d.post(t, "/anthropic/v1/messages", `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d after the documented fix, want 200\n%s", resp.StatusCode, body)
	}
	if last := e.upstream.keys()[len(e.upstream.keys())-1]; last != "right-key" {
		t.Errorf("upstream saw %q, want the replacement key", last)
	}
}

// An account pooled while the daemon is up should be usable immediately.
func TestAnAccountAddedLaterIsPickedUp(t *testing.T) {
	e := newEnv(t)
	e.pool("first")
	d := e.serve()
	d.post(t, "/anthropic/v1/messages", `{}`)

	e.mustRun("add-key", "anthropic", "--key", "second", "--id", "second",
		"--base-url", e.upstream.url(), "--priority", "0")
	e.mustRun("disable", "first")

	e.upstream.reset()
	resp, body := d.post(t, "/anthropic/v1/messages", `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the newly pooled account used\n%s", resp.StatusCode, body)
	}
	if got := e.upstream.keys(); len(got) != 1 || got[0] != "second" {
		t.Errorf("upstream saw %v, want only the new account", got)
	}
}

// An imported credential is the one the CLI is currently using, and both sides
// rotate the refresh token when they renew — so whichever renews first retires
// the other's copy. A long-lived token is a separate credential the CLI never
// touches, which is the way out of that race.
func TestLongLivedTokenIsPooledAsASubscription(t *testing.T) {
	e := newEnv(t)

	cmd := exec.Command(binary, "add-token", "anthropic", "--token", "-", "--id", "longlived",
		"--base-url", e.upstream.url())
	cmd.Env = e.environ()
	cmd.Stdin = strings.NewReader("sk-ant-oat01-long-lived-value\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("add-token: %v\n%s", err, out)
	}
	mustContain(t, string(out), "not go stale", "add-token")

	list := e.mustRun("list").out()
	mustContain(t, list, "long-lived", "list")
	mustContain(t, list, "subscription", "list") // spent before metered keys
	mustNotContain(t, list, "long-lived-value", "list")

	// It has to reach the upstream as a bearer token, not as an api key.
	d := e.serve()
	resp, body := d.post(t, "/anthropic/v1/messages", `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	seen := e.upstream.seen()
	if got := seen[0].Header.Get("Authorization"); got != "Bearer sk-ant-oat01-long-lived-value" {
		t.Errorf("Authorization = %q, want the token as a bearer", got)
	}
	if got := seen[0].Header.Get("X-Api-Key"); got != "" {
		t.Errorf("X-Api-Key = %q, want a token sent as a bearer instead", got)
	}
}

// Nothing to sign into and no key to swap: the remedy is a new token.
func TestARejectedLongLivedTokenSaysToIssueANewOne(t *testing.T) {
	e := newEnv(t)
	cmd := exec.Command(binary, "add-token", "anthropic", "--token", "-", "--id", "longlived",
		"--base-url", e.upstream.url())
	cmd.Env = e.environ()
	cmd.Stdin = strings.NewReader("sk-ant-oat01-revoked\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("add-token: %v\n%s", err, out)
	}
	e.upstream.handle(func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"OAuth access token has been revoked."}}`)
	})
	d := e.serve()

	_, body := d.post(t, "/anthropic/v1/messages", `{}`)
	mustContain(t, body, "OAuth access token has been revoked", "the refusal")
	mustContain(t, body, "claude setup-token", "the refusal")
	mustContain(t, body, "agentswap add-token", "the refusal")
}
