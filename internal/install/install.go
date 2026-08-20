// Package install wires Claude Code and Codex to a running agentswap daemon.
//
// Both edits are reversible and neither replaces an existing configuration:
// the Claude settings file is merged key by key, and Codex gets an additive,
// delimited provider block plus a small profile overlay. A timestamped backup
// is written before either file is changed.
package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

	profileBeginMarker = "# >>> agentswap profile >>>"
	profileEndMarker   = "# <<< agentswap profile <<<"
	profileCreatedFile = "# agentswap-created-profile-file"
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
	Paths   []string // Path followed by any related files changed by the plan.
	Action  string   // "create" or "update"
	Preview string
}

// ClaudeSettingsPath returns the user-level Claude Code settings file.
//
// CLAUDE_CONFIG_DIR is honored because Claude Code honors it. Writing to
// ~/.claude while the CLI reads somewhere else would leave `agentswap install`
// reporting success having configured nothing, and `doctor` insisting the CLI
// is not wired up no matter how many times you run it.
func ClaudeSettingsPath() (string, error) {
	dir, err := ClaudeConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings.json"), nil
}

// ClaudeConfigDir is where Claude Code keeps its settings and credentials.
func ClaudeConfigDir() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

// CodexConfigPath returns the Codex config file.
func CodexConfigPath() (string, error) {
	home, err := codexHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config.toml"), nil
}

// CodexProfilePath returns the v2 profile overlay used by modern Codex
// releases. Keeping this separate from config.toml avoids the legacy
// [profiles.<name>] table, which current Codex rejects when --profile is used.
func CodexProfilePath() (string, error) {
	home, err := codexHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ProfileName+".config.toml"), nil
}

func codexHome() (string, error) {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

// CodexProfileUsesAgentSwap reports whether the profile selected by
// `--profile agentswap` actually selects Agent Swap's provider. Doctor uses
// this alongside the base provider URL: either half without the other would
// make a handoff bypass the proxy or fail at launch time.
func CodexProfileUsesAgentSwap() (bool, error) {
	path, err := CodexProfilePath()
	if err != nil {
		return false, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	provider, ok := topLevelTOMLString(string(b), "model_provider")
	return ok && provider == ProfileName, nil
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
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

// CodexBlock is the additive provider configuration agentswap appends to the
// base Codex config. The profile selector itself lives in CodexProfileBlock;
// modern Codex reads profiles from <name>.config.toml and rejects the old
// [profiles.<name>] table.
func CodexBlock(addr string) string {
	var b strings.Builder
	b.WriteString(beginMarker + "\n")
	b.WriteString("# Added by `agentswap install`. Remove with `agentswap uninstall`.\n")
	b.WriteString("# Select it explicitly with:  codex --profile " + ProfileName + "\n")
	fmt.Fprintf(&b, "[model_providers.%s]\n", ProfileName)
	fmt.Fprintf(&b, "name = %q\n", ProfileName)
	fmt.Fprintf(&b, "base_url = \"http://%s/openai\"\n", addr)
	b.WriteString("wire_api = \"responses\"\n")
	b.WriteString("requires_openai_auth = true\n\n")
	b.WriteString(endMarker + "\n")
	return b.String()
}

// codexProfileBlock is the complete managed portion of the v2 profile file.
// It deliberately contains only the provider selector: model, sandbox, and
// other options remain inherited from the user's base config unless they add
// them to this profile themselves.
func codexProfileBlock(createdFile bool) string {
	var b strings.Builder
	b.WriteString(profileBeginMarker + "\n")
	b.WriteString("# Added by `agentswap install`. Remove with `agentswap uninstall`.\n")
	if createdFile {
		b.WriteString(profileCreatedFile + "\n")
	}
	b.WriteString("model_provider = \"" + ProfileName + "\"\n")
	b.WriteString(profileEndMarker + "\n")
	return b.String()
}

// InstallCodex appends (or refreshes) agentswap's block in the Codex config.
func InstallCodex(addr string, dryRun bool) (*Plan, error) {
	path, err := CodexConfigPath()
	if err != nil {
		return nil, err
	}
	profilePath, err := CodexProfilePath()
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
	if hasTOMLTable(body, "model_providers."+ProfileName) {
		return nil, fmt.Errorf("%s already defines the %q Codex provider; remove or rename that block before running agentswap install", path, ProfileName)
	}
	if hasTOMLTable(body, "profiles."+ProfileName) || hasTopLevelTOMLKey(body, "profile") {
		return nil, fmt.Errorf("%s contains a user-owned legacy Codex profile selector; migrate it to a <name>.config.toml file before running agentswap install", path)
	}
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if body != "" {
		body += "\n"
	}
	out := body + CodexBlock(addr)

	profileExisting := ""
	profileExisted := false
	if b, err := os.ReadFile(profilePath); err == nil {
		profileExisting = string(b)
		profileExisted = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", profilePath, err)
	}
	profileBody := stripProfileBlock(profileExisting)
	if profileBody != "" && hasTopLevelTOMLKey(profileBody, "model_provider") {
		return nil, fmt.Errorf("%s already selects a Codex provider; move that configuration before installing the %s profile", profilePath, ProfileName)
	}
	// A TOML document cannot return to its root after entering a table, so the
	// managed top-level selector must precede user-owned tables. Prepending also
	// lets uninstall retain the user's file byte for byte.
	profileBlock := codexProfileBlock(!profileExisted || strings.Contains(profileExisting, profileCreatedFile))
	profileOut := profileBlock
	if profileBody != "" {
		profileOut += "\n" + profileBody
	}

	plan := &Plan{Path: path, Paths: []string{path, profilePath}, Action: action,
		Preview: CodexBlock(addr) + "\n# " + profilePath + "\n" + profileBlock}
	if dryRun {
		return plan, nil
	}
	if err := backup(path); err != nil {
		return nil, err
	}
	if err := backup(profilePath); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(profilePath, []byte(profileOut), 0o600); err != nil {
		return nil, err
	}
	return plan, nil
}

// UninstallCodex removes agentswap's delimited provider and profile blocks,
// leaving user-owned configuration as is.
func UninstallCodex() error {
	path, err := CodexConfigPath()
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		out := stripBlock(string(b))
		if out != string(b) {
			if err := backup(path); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
				return err
			}
		}
	}

	profilePath, err := CodexProfilePath()
	if err != nil {
		return err
	}
	pb, err := os.ReadFile(profilePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	removeCreatedFile := strings.Contains(string(pb), profileCreatedFile)
	profileOut := stripProfileBlock(string(pb))
	if profileOut == string(pb) {
		return nil
	}
	if err := backup(profilePath); err != nil {
		return err
	}
	if removeCreatedFile && strings.TrimSpace(profileOut) == "" {
		return os.Remove(profilePath)
	}
	return os.WriteFile(profilePath, []byte(profileOut), 0o600)
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

func stripProfileBlock(s string) string {
	start := strings.Index(s, profileBeginMarker)
	if start < 0 {
		return s
	}
	rest := s[start:]
	end := strings.Index(rest, profileEndMarker)
	if end < 0 {
		// Match stripBlock's fail-safe behavior for an interrupted write.
		return strings.TrimSuffix(s[:start], "\n")
	}
	end += start + len(profileEndMarker)
	if strings.HasPrefix(s[end:], "\r\n") {
		end += 2
	} else if strings.HasPrefix(s[end:], "\n") {
		end++
	}
	prefix, suffix := s[:start], s[end:]
	if start == 0 {
		// Drop the one blank line InstallCodex inserts between its block and
		// the original file, without normalizing any user-owned bytes.
		suffix = strings.TrimPrefix(suffix, "\n")
	} else {
		prefix = strings.TrimSuffix(prefix, "\n")
	}
	return prefix + suffix
}

// hasTOMLTable and hasTopLevelTOMLKey intentionally handle only the simple
// declarations agentswap owns. They are conflict guards, not a TOML parser;
// comments and whitespace around the declaration are ignored.
func hasTOMLTable(body, table string) bool {
	want := "[" + table + "]"
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		if line == want {
			return true
		}
	}
	return false
}

func hasTopLevelTOMLKey(body, key string) bool {
	_, ok := topLevelTOMLValue(body, key)
	return ok
}

func topLevelTOMLString(body, key string) (string, bool) {
	value, ok := topLevelTOMLValue(body, key)
	if !ok {
		return "", false
	}
	if unquoted, err := strconv.Unquote(value); err == nil {
		return unquoted, true
	}
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return value[1 : len(value)-1], true
	}
	return "", false
}

func topLevelTOMLValue(body, key string) (string, bool) {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		if strings.HasPrefix(line, "[") {
			// TOML keys after a table header belong to that table; root keys
			// cannot resume later in the document.
			return "", false
		}
		if strings.HasPrefix(line, key) && strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(line, key)), "=") {
			rest := strings.TrimSpace(strings.TrimPrefix(line, key))
			return strings.TrimSpace(strings.TrimPrefix(rest, "=")), true
		}
	}
	return "", false
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
