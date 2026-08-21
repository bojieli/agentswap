// Package e2e drives the real agentswap binary as a subprocess.
//
// The unit tests cover decisions; these cover the thing a user actually runs.
// Everything here goes through argv, exit codes, files on disk and HTTP — no
// internal packages are imported, deliberately, so a refactor that breaks the
// product is not hidden by a refactor of the tests.
package e2e

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// binary is the compiled agentswap under test, built once for the package.
var binary string

func TestMain(m *testing.M) {
	// testing.Short is only readable after the flags are parsed, and TestMain
	// runs before that unless we do it ourselves.
	flag.Parse()
	if testing.Short() {
		// -short is how a contributor skips the slow, subprocess-heavy pass.
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "agentswap-e2e-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	binary = filepath.Join(dir, "agentswap")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}

	args := []string{"build", "-o", binary}
	// Coverage from a subprocess needs an instrumented build plus GOCOVERDIR.
	// Without it the CLI layer looks untested even though it is what these
	// tests exercise most.
	if coverDir() != "" {
		args = append(args, "-cover", "-coverpkg=github.com/bojieli/agentswap/...")
	}
	args = append(args, "./cmd/agentswap")

	build := exec.Command("go", args...)
	build.Dir = ".."
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: building agentswap failed: %v\n%s", err, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// coverDir is where an instrumented binary should drop its coverage profiles.
// CI sets it; a plain `go test` run leaves it empty and skips instrumentation.
func coverDir() string {
	d := os.Getenv("E2E_COVERDIR")
	if d == "" {
		return ""
	}
	abs, err := filepath.Abs(d)
	if err != nil {
		return ""
	}
	_ = os.MkdirAll(abs, 0o755)
	return abs
}

// env is one isolated agentswap installation: its own config directory, its own
// stand-ins for the CLIs' config directories, and its own fake upstream.
type env struct {
	t        *testing.T
	home     string // AGENTSWAP_HOME
	claude   string // CLAUDE_CONFIG_DIR
	codex    string // CODEX_HOME
	upstream *upstream

	// configAddr is set when a test pinned the listen address in config.json.
	configAddr string

	mu      sync.Mutex
	daemons []*daemonProc
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	e := &env{
		t:      t,
		home:   filepath.Join(base, "agentswap"),
		claude: filepath.Join(base, "claude"),
		codex:  filepath.Join(base, "codex"),
	}
	for _, d := range []string{e.home, e.claude, e.codex} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	e.upstream = newUpstream(t)
	t.Cleanup(e.stopAll)
	return e
}

func (e *env) environ() []string {
	return append(os.Environ(),
		"AGENTSWAP_HOME="+e.home,
		"CLAUDE_CONFIG_DIR="+e.claude,
		"CODEX_HOME="+e.codex,
		// Keep a stray real login from leaking into an import test.
		"CLAUDE_CREDENTIALS_PATH="+filepath.Join(e.claude, ".credentials.json"),
		// A test that asserts no daemon is running must not be able to reach
		// one. Every other root is redirected above, but the listen address
		// would otherwise fall back to the compiled-in default and find the
		// developer's own daemon. Port 1 needs root to bind, so nothing in the
		// test run — or on the machine — can be listening there, and the probe
		// is refused immediately rather than waiting out a timeout.
		"AGENTSWAP_ADDR=127.0.0.1:1",
		"GOCOVERDIR="+coverDir(),
	)
}

// result is the outcome of one CLI invocation.
type result struct {
	stdout string
	stderr string
	code   int
}

func (r result) out() string { return r.stdout + r.stderr }

// run invokes the CLI and returns its output, whatever the exit code. Commands
// that are supposed to fail are part of the contract too.
func (e *env) run(args ...string) result {
	e.t.Helper()
	return e.runEnv(nil, args...)
}

// runEnv is run with extra environment, for the commands that read some.
func (e *env) runEnv(extra []string, args ...string) result {
	e.t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = append(e.environ(), extra...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			e.t.Fatalf("running %v: %v", args, err)
		}
		code = exit.ExitCode()
	}
	return result{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// mustRun fails the test if the command did not succeed.
func (e *env) mustRun(args ...string) result {
	e.t.Helper()
	r := e.run(args...)
	if r.code != 0 {
		e.t.Fatalf("agentswap %s exited %d\n%s", strings.Join(args, " "), r.code, r.out())
	}
	return r
}

// daemonProc is a running `agentswap serve`.
type daemonProc struct {
	cmd  *exec.Cmd
	addr string
	log  *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// pinAddr writes a concrete listen address into config.json and returns it, for
// the tests where the daemon and the CLI configuration have to agree on a port.
// Everything else lets the kernel pick.
func (e *env) pinAddr() string {
	e.t.Helper()
	addr := freePort(e.t)
	e.configAddr = addr

	path := filepath.Join(e.home, "config.json")
	cfg := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	cfg["addr"] = addr
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		e.t.Fatalf("marshal config: %v", err)
	}
	writeFile(e.t, path, string(b))
	return addr
}

// serve starts a daemon and waits for it to answer. Unless the test pinned an
// address, the kernel picks the port and the daemon publishes what it got.
func (e *env) serve(extraArgs ...string) *daemonProc {
	e.t.Helper()

	args := []string{"serve"}
	if e.configAddr == "" {
		args = append(args, "--addr", "127.0.0.1:0")
	}
	args = append(args, extraArgs...)
	cmd := exec.Command(binary, args...)
	cmd.Env = e.environ()
	log := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		e.t.Fatalf("start daemon: %v", err)
	}

	d := &daemonProc{cmd: cmd, log: log}
	e.mu.Lock()
	e.daemons = append(e.daemons, d)
	e.mu.Unlock()

	// The daemon publishes its real address once bound, which is the only way
	// to learn a port the kernel chose.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if addr := e.publishedAddr(); addr != "" && probe(addr) {
			d.addr = addr
			return d
		}
		if cmd.ProcessState != nil {
			e.t.Fatalf("daemon exited before it was ready:\n%s", log.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("daemon never became ready:\n%s", log.String())
	return nil
}

// publishedAddr reads the address the daemon recorded for other commands.
func (e *env) publishedAddr() string {
	b, err := os.ReadFile(filepath.Join(e.home, "daemon.json"))
	if err != nil {
		return ""
	}
	var info struct {
		Addr string `json:"addr"`
	}
	if json.Unmarshal(b, &info) != nil {
		return ""
	}
	return info.Addr
}

func probe(addr string) bool {
	c := &http.Client{Timeout: time.Second}
	resp, err := c.Get("http://" + addr + "/_agentswap/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// gracefulStopSupported reports whether this platform lets a test ask the
// daemon to shut down cleanly.
//
// A Windows console does deliver Ctrl-C as os.Interrupt, so a user pressing it
// gets the same clean shutdown as everywhere else. What is not available is
// sending that event to a child process from a test: it needs
// GenerateConsoleCtrlEvent and a shared console group, which a test binary does
// not have. So on Windows stop() is a hard kill, and the tests that assert on
// clean-shutdown behaviour say so rather than pretending.
func gracefulStopSupported() bool { return runtime.GOOS != "windows" }

// stop shuts the daemon down the way a user would, and waits for it.
func (d *daemonProc) stop(t *testing.T) {
	t.Helper()
	if d.cmd.Process == nil || d.cmd.ProcessState != nil {
		return
	}
	if runtime.GOOS == "windows" {
		_ = d.cmd.Process.Kill()
	} else {
		_ = d.cmd.Process.Signal(os.Interrupt)
	}
	done := make(chan struct{})
	go func() { _ = d.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		_ = d.cmd.Process.Kill()
		<-done
		t.Errorf("daemon did not shut down on a signal:\n%s", d.log.String())
	}
}

func (e *env) stopAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, d := range e.daemons {
		if d.cmd.Process != nil && d.cmd.ProcessState == nil {
			_ = d.cmd.Process.Kill()
			_ = d.cmd.Wait()
		}
	}
}

// post sends a request through the proxy the way a CLI would.
func (d *daemonProc) post(t *testing.T, path, body string) (*http.Response, string) {
	t.Helper()
	return d.postWith(t, path, body, nil, 60*time.Second)
}

func (d *daemonProc) postWith(t *testing.T, path, body string, headers map[string]string, timeout time.Duration) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+d.addr+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out)
}

// upstream is a stand-in for api.anthropic.com or the Codex backend.
type upstream struct {
	srv *httptest.Server

	mu       sync.Mutex
	handler  func(w http.ResponseWriter, r *http.Request, req recorded)
	requests []recorded
}

// recorded is one request the upstream saw.
type recorded struct {
	Path   string
	Method string
	Header http.Header
	Body   string
	// Key is whichever credential arrived, whatever form it took, so a test can
	// assert which account served a request without knowing the lane.
	Key string
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec := recorded{
			Path: r.URL.Path, Method: r.Method, Header: r.Header.Clone(),
			Body: string(body), Key: credential(r),
		}
		u.mu.Lock()
		u.requests = append(u.requests, rec)
		h := u.handler
		u.mu.Unlock()

		if h == nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		}
		h(w, r, rec)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func credential(r *http.Request) string {
	if k := r.Header.Get("X-Api-Key"); k != "" {
		return k
	}
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (u *upstream) url() string { return u.srv.URL }

func (u *upstream) handle(h func(w http.ResponseWriter, r *http.Request, req recorded)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.handler = h
}

func (u *upstream) seen() []recorded {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]recorded(nil), u.requests...)
}

// keys returns the credential used for each request, in order — the shortest
// way to say "it rotated".
func (u *upstream) keys() []string {
	var out []string
	for _, r := range u.seen() {
		out = append(out, r.Key)
	}
	return out
}

func (u *upstream) reset() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.requests = nil
}

// writeFile is a fixture helper for credential and config files.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// freePort returns a port nothing is listening on, for the cases that need an
// address before a daemon exists.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().String()
}

func mustContain(t *testing.T, got, want, what string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("%s: want %q in output, got:\n%s", what, want, got)
	}
}

func mustNotContain(t *testing.T, got, unwanted, what string) {
	t.Helper()
	if strings.Contains(got, unwanted) {
		t.Errorf("%s: did not want %q in output, got:\n%s", what, unwanted, got)
	}
}
