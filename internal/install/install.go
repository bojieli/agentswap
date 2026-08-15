// Package install wires Claude Code and Codex to a running agentswap daemon.
//
// Both edits are reversible and neither replaces an existing configuration:
// the Claude settings file is merged key by key, and the Codex config gains an
// additive, delimited block. A timestamped backup is written before either.
package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Markers delimit the block agentswap owns inside the user's Codex config, so
// uninstall can remove exactly what was added and nothing else.
const (
	beginMarker = "# >>> agentswap >>>"
	endMarker   = "# <<< agentswap <<<"

	// ProfileName is the Codex profile and provider id agentswap registers.
	ProfileName = "agentswap"
)

// AuthTokenPlaceholder is what Claude Code is told to send. The proxy discards
// whatever credential arrives and substitutes one from the pool, but the CLI
// still needs a non-empty value to consider itself configured.
const AuthTokenPlaceholder = "agentswap-managed"

// minClientTimeout is the floor for how long the CLI is told to wait. Two
// hours comfortably covers a long streaming answer on a pool that is not
// parked at all.
const minClientTimeout = 2 * time.Hour

// clientTimeoutMargin keeps the client's deadline clear of the daemon's, so a
// park that ends in an answer is not cut off a moment before it arrives.
const clientTimeoutMargin = 5 * time.Minute

// clientTimeout is how long the CLI is told to wait for a response.
//
// It has to outlast park.max_hold: a parked request deliberately sends nothing
// until quota returns, so a client that gives up first converts a wait that was
// about to succeed into a failure — and does it without the daemon ever
// learning why.
func clientTimeout(maxHold time.Duration) time.Duration {
	if d := maxHold + clientTimeoutMargin; d > minClientTimeout {
		return d
	}
	return minClientTimeout
}

// Plan describes an edit without performing it, so `install --dry-run` can
// show the user exactly what will change.
type Plan struct {
	Path    string
	Action  string // "create" or "update"
	Preview string
}

// ClaudeSettingsPath returns the user-level Claude Code settings file.
func ClaudeSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// CodexConfigPath returns the Codex config file.
func CodexConfigPath() (string, error) {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return filepath.Join(h, "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

// ClaudeEnv is the environment block agentswap manages inside settings.json.
func ClaudeEnv(addr string, maxHold time.Duration) map[string]string {
	return map[string]string{
		envBaseURL:   baseURLFor(addr),
		envAuthToken: AuthTokenPlaceholder,
		envTimeout:   fmt.Sprint(clientTimeout(maxHold).Milliseconds()),
	}
}

// The keys agentswap owns inside the user's env block.
const (
	envBaseURL   = "ANTHROPIC_BASE_URL"
	envAuthToken = "ANTHROPIC_AUTH_TOKEN"
	envTimeout   = "API_TIMEOUT_MS"
)

func baseURLFor(addr string) string { return "http://" + addr + "/anthropic" }

// InstallClaude merges agentswap's environment into Claude Code's settings,
// leaving every other key untouched.
func InstallClaude(addr string, maxHold time.Duration, dryRun bool) (*Plan, error) {
	path, err := ClaudeSettingsPath()
	if err != nil {
		return nil, err
	}

	settings := map[string]any{}
	action := "create"
	if b, err := os.ReadFile(path); err == nil {
		action = "update"
		if err := json.Unmarshal(b, &settings); err != nil {
			return nil, fmt.Errorf("parse %s: %w (fix or move it, then retry)", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	env, _ := settings["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	for k, v := range ClaudeEnv(addr, maxHold) {
		env[k] = v
	}
	settings["env"] = env

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, err
	}
	out = append(out, '\n')

	plan := &Plan{Path: path, Action: action, Preview: string(out)}
	if dryRun {
		return plan, nil
	}
	if err := backup(path); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return plan, os.WriteFile(path, out, 0o600)
}

// UninstallClaude removes only the keys agentswap added.
func UninstallClaude(addr string) error {
	path, err := ClaudeSettingsPath()
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	settings := map[string]any{}
	if err := json.Unmarshal(b, &settings); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	env, _ := settings["env"].(map[string]any)
	if env == nil {
		return nil
	}
	// Only touch a block we recognise as our own. A user who has since pointed
	// the CLI somewhere else deliberately should keep their setting.
	//
	// The base URL is what identifies the block: it names our address and
	// nothing else would. Once it matches, the other two keys are ours by
	// association — matching those on value as well would strand API_TIMEOUT_MS
	// whenever park.max_hold changed between install and uninstall, since the
	// timeout is derived from it.
	if got, ok := env[envBaseURL].(string); !ok || got != baseURLFor(addr) {
		return nil
	}
	for _, k := range []string{envBaseURL, envAuthToken, envTimeout} {
		delete(env, k)
	}
	if len(env) == 0 {
		delete(settings, "env")
	} else {
		settings["env"] = env
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := backup(path); err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o600)
}

// CodexBlock is the additive configuration agentswap appends. It registers a
// provider and a profile rather than changing model_provider at top level,
// because appending is safe and rewriting an existing key is not.
func CodexBlock(addr string) string {
	var b strings.Builder
	b.WriteString(beginMarker + "\n")
	b.WriteString("# Added by `agentswap install`. Remove with `agentswap uninstall`.\n")
	b.WriteString("# Use it with:  codex --profile " + ProfileName + "\n")
	fmt.Fprintf(&b, "[model_providers.%s]\n", ProfileName)
	fmt.Fprintf(&b, "name = %q\n", ProfileName)
	fmt.Fprintf(&b, "base_url = \"http://%s/openai\"\n", addr)
	b.WriteString("wire_api = \"responses\"\n")
	b.WriteString("requires_openai_auth = true\n\n")
	fmt.Fprintf(&b, "[profiles.%s]\n", ProfileName)
	fmt.Fprintf(&b, "model_provider = %q\n", ProfileName)
	b.WriteString(endMarker + "\n")
	return b.String()
}

// InstallCodex appends (or refreshes) agentswap's block in the Codex config.
func InstallCodex(addr string, dryRun bool) (*Plan, error) {
	path, err := CodexConfigPath()
	if err != nil {
		return nil, err
	}

	existing := ""
	action := "create"
	if b, err := os.ReadFile(path); err == nil {
		existing = string(b)
		action = "update"
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	body := stripBlock(existing)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if body != "" {
		body += "\n"
	}
	out := body + CodexBlock(addr)

	plan := &Plan{Path: path, Action: action, Preview: CodexBlock(addr)}
	if dryRun {
		return plan, nil
	}
	if err := backup(path); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return plan, os.WriteFile(path, []byte(out), 0o600)
}

// UninstallCodex removes agentswap's delimited block, leaving the rest as is.
func UninstallCodex() error {
	path, err := CodexConfigPath()
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	out := stripBlock(string(b))
	if out == string(b) {
		return nil
	}
	if err := backup(path); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(out), 0o600)
}

// stripBlock removes the delimited agentswap region, if present, restoring the
// file to exactly what it was before the block was appended. An unclosed begin
// marker is treated as extending to end of file rather than being ignored, so
// a truncated write cannot leave the block un-removable.
func stripBlock(s string) string {
	start := strings.Index(s, beginMarker)
	if start < 0 {
		return s
	}
	var tail string
	rest := s[start:]
	if end := strings.Index(rest, endMarker); end >= 0 {
		tail = rest[end+len(endMarker):]
	}

	// Drop the blank-line separator that install inserted, then re-terminate:
	// a config file that no longer ends in a newline is a gratuitous diff.
	out := strings.TrimRight(s[:start], "\n")
	if tail = strings.TrimLeft(tail, "\n"); tail != "" {
		out += "\n" + tail
	}
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out
}

// backup copies path alongside itself before it is modified. Losing a hand
// tuned settings file to a tool that was supposed to help is not acceptable.
func backup(path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	dst := fmt.Sprintf("%s.agentswap-backup-%s", path, time.Now().Format("20060102-150405"))
	return os.WriteFile(dst, b, 0o600)
}
