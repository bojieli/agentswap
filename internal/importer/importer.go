// Package importer reads credentials and active provider overrides already on
// disk for `claude` and `codex`, so adopting agentswap does not mean rebuilding
// a working CLI configuration by hand.
package importer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/bojieli/agentswap/internal/install"
	"github.com/bojieli/agentswap/internal/store"
)

// ErrNoCredentials means the CLI is not logged in on this machine.
var ErrNoCredentials = errors.New("no credentials found")

// claudeCredentials mirrors ~/.claude/.credentials.json. Only the fields
// agentswap needs are declared; the file holds unrelated MCP tokens too, which
// are deliberately left alone.
type claudeCredentials struct {
	ClaudeAIOAuth struct {
		AccessToken      string   `json:"accessToken"`
		RefreshToken     string   `json:"refreshToken"`
		ExpiresAt        int64    `json:"expiresAt"`
		Scopes           []string `json:"scopes"`
		SubscriptionType string   `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
}

// codexAuth mirrors ~/.codex/auth.json, which holds either an API key or a
// ChatGPT token set depending on how the user logged in.
type codexAuth struct {
	APIKey string `json:"OPENAI_API_KEY"`
	Tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
}

type claudeSettings struct {
	Env map[string]any `json:"env"`
}

// codexProvider is the subset of a [model_providers.<id>] table needed to
// preserve the active custom provider during import.
type codexProvider struct {
	ID                 string
	BaseURL            string
	EnvKey             string
	BearerToken        string
	RequiresOpenAIAuth bool
}

// ClaudeCredentialPath returns the location of Claude Code's credential file.
func ClaudeCredentialPath() (string, error) {
	path, _, err := claudeCredentialPath()
	return path, err
}

// claudeCredentialPath also reports whether the location was chosen by the
// user. That decides whether falling back to the Keychain is helpful or
// dangerous: someone who named a file means that file, and quietly importing a
// different account from the Keychain instead would pool the wrong credential.
func claudeCredentialPath() (path string, explicit bool, err error) {
	if p := os.Getenv("CLAUDE_CREDENTIALS_PATH"); p != "" {
		return p, true, nil
	}
	// CLAUDE_CONFIG_DIR moves the whole directory, credentials included. It is
	// not an explicit choice of *file*, so the Keychain fallback still applies.
	dir, err := install.ClaudeConfigDir()
	if err != nil {
		return "", false, err
	}
	return filepath.Join(dir, ".credentials.json"), false, nil
}

// CodexAuthPath returns the default location of Codex's auth file.
func CodexAuthPath() (string, error) {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return filepath.Join(h, "auth.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "auth.json"), nil
}

// ImportClaude builds an account from the current Claude Code login. The
// account is returned unnamed: only the caller knows how many credentials this
// import turned up, and that decides whether names need disambiguating.
func ImportClaude() (*store.Account, error) {
	raw, err := readClaudeCredentials()
	if err != nil {
		return nil, err
	}
	var c claudeCredentials
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse claude credentials: %w", err)
	}
	o := c.ClaudeAIOAuth
	if o.AccessToken == "" {
		return nil, fmt.Errorf("%w: claude credentials hold no oauth token; run `claude` and log in first", ErrNoCredentials)
	}
	return &store.Account{
		Lane: store.LaneAnthropic, Kind: store.KindOAuth, Enabled: true,
		AccessToken:      o.AccessToken,
		RefreshToken:     o.RefreshToken,
		ExpiresAt:        o.ExpiresAt,
		Scopes:           o.Scopes,
		SubscriptionType: o.SubscriptionType,
	}, nil
}

// ImportClaudeAll returns both independent credential sources Claude Code can
// be using at once: its native subscription login and a provider override in
// settings.json. The latter must retain its base URL and header style or an
// apparently successful import would send the secret to the wrong upstream.
func ImportClaudeAll() ([]*store.Account, error) {
	var out []*store.Account

	if native, err := ImportClaude(); err == nil {
		out = append(out, native)
	} else if !errors.Is(err, ErrNoCredentials) {
		return nil, err
	}

	configured, err := importClaudeProvider()
	if err != nil {
		return nil, err
	}
	if configured != nil {
		out = append(out, configured)
	}
	if len(out) == 0 {
		return nil, ErrNoCredentials
	}
	return out, nil
}

func importClaudeProvider() (*store.Account, error) {
	path, err := install.ClaudeSettingsPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var settings claudeSettings
	if err := json.Unmarshal(b, &settings); err != nil {
		return nil, fmt.Errorf("parse claude settings: %w", err)
	}
	baseURL := stringValue(settings.Env["ANTHROPIC_BASE_URL"])
	if baseURL == "" || isAgentSwapURL(baseURL, "/anthropic") {
		return nil, nil
	}

	key := stringValue(settings.Env["ANTHROPIC_AUTH_TOKEN"])
	style := store.AuthStyleBearer
	if key == "" {
		key = stringValue(settings.Env["ANTHROPIC_API_KEY"])
		style = store.AuthStyleXAPIKey
	}
	if key == "" || key == install.AuthTokenPlaceholder {
		return nil, nil
	}
	return &store.Account{
		Lane: store.LaneAnthropic, Kind: store.KindAPIKey, Enabled: true,
		APIKey: key, BaseURL: baseURL, AuthStyle: style,
	}, nil
}

// readClaudeCredentials reads the credential file, falling back to the macOS
// Keychain where Claude Code stores it instead of on disk.
func readClaudeCredentials() ([]byte, error) {
	path, explicit, err := claudeCredentialPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err == nil {
		return b, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if runtime.GOOS == "darwin" && !explicit {
		if b, kerr := keychainCredentials(); kerr == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("%w at %s; run `claude` and log in first", ErrNoCredentials, path)
}

// keychainCredentials pulls Claude Code's credentials out of the macOS
// Keychain. This may prompt the user for access, which is expected and correct.
func keychainCredentials() ([]byte, error) {
	out, err := exec.Command("security", "find-generic-password",
		"-s", "Claude Code-credentials", "-w").Output()
	if err != nil {
		return nil, fmt.Errorf("read keychain: %w", err)
	}
	return []byte(strings.TrimSpace(string(out))), nil
}

// ImportCodex builds the primary account from the current Codex login. Login
// uses this singular form while waiting for a newly minted subscription;
// `agentswap import` uses ImportCodexAll so no simultaneous provider key is
// discarded.
func ImportCodex() (*store.Account, error) {
	all, err := ImportCodexAll()
	if err != nil {
		return nil, err
	}
	return all[0], nil
}

// ImportCodexAll preserves a native ChatGPT subscription and the selected
// custom provider independently when both credentials are available. A custom
// provider key can come from auth.json (requires_openai_auth), an env_key, or
// the discouraged direct bearer-token setting supported by Codex itself.
func ImportCodexAll() ([]*store.Account, error) {
	path, err := CodexAuthPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	missingAuth := errors.Is(err, os.ErrNotExist)
	if err != nil && !missingAuth {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var a codexAuth
	if !missingAuth {
		if err := json.Unmarshal(b, &a); err != nil {
			return nil, fmt.Errorf("parse codex auth: %w", err)
		}
	}

	provider, err := selectedCodexProvider()
	if err != nil {
		return nil, err
	}

	var out []*store.Account
	if a.Tokens.AccessToken != "" {
		out = append(out, &store.Account{
			Lane: store.LaneOpenAI, Kind: store.KindOAuth, Enabled: true,
			AccessToken:      a.Tokens.AccessToken,
			RefreshToken:     a.Tokens.RefreshToken,
			ChatGPTAccountID: a.Tokens.AccountID,
		})
	}

	usedAuthKey := false
	if provider != nil {
		key := provider.BearerToken
		if key == "" && provider.EnvKey != "" {
			key = os.Getenv(provider.EnvKey)
		}
		if key == "" && provider.RequiresOpenAIAuth {
			key = a.APIKey
			usedAuthKey = key != ""
		}
		if key != "" {
			out = append(out, &store.Account{
				Lane: store.LaneOpenAI, Kind: store.KindAPIKey, Enabled: true,
				APIKey: key, BaseURL: provider.BaseURL, AuthStyle: store.AuthStyleBearer,
			})
		}
	}
	if a.APIKey != "" && !usedAuthKey {
		out = append(out, &store.Account{
			Lane: store.LaneOpenAI, Kind: store.KindAPIKey, Enabled: true,
			APIKey: a.APIKey,
		})
	}
	if len(out) == 0 {
		if missingAuth {
			return nil, fmt.Errorf("%w at %s; run `codex login` first", ErrNoCredentials, path)
		}
		return nil, fmt.Errorf("%w: codex auth holds neither tokens nor a usable api key", ErrNoCredentials)
	}
	return out, nil
}

// selectedCodexProvider reads only the small, intentionally simple portion of
// config.toml that chooses a provider and describes its authentication. The
// project has no third-party dependencies, so a full TOML parser would be a
// disproportionate and security-sensitive addition for four scalar fields.
func selectedCodexProvider() (*codexProvider, error) {
	path, err := install.CodexConfigPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	lines := strings.Split(string(b), "\n")

	providerID := ""
	for _, raw := range lines {
		line := strings.TrimSpace(stripTOMLComment(raw))
		if strings.HasPrefix(line, "[") {
			break
		}
		key, value, ok := tomlAssignment(line)
		if ok && key == "model_provider" {
			providerID, ok = tomlString(value)
			if !ok {
				return nil, fmt.Errorf("parse %s: model_provider is not a string", path)
			}
			break
		}
	}
	if providerID == "" || providerID == "openai" || providerID == install.ProfileName {
		return nil, nil
	}

	p := &codexProvider{ID: providerID}
	inProvider := false
	foundTable := false
	for _, raw := range lines {
		line := strings.TrimSpace(stripTOMLComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			table := strings.TrimSpace(line[1 : len(line)-1])
			id, ok := codexProviderTableID(table)
			inProvider = ok && id == providerID
			foundTable = foundTable || inProvider
			continue
		}
		if !inProvider {
			continue
		}
		key, value, ok := tomlAssignment(line)
		if !ok {
			continue
		}
		switch key {
		case "base_url":
			p.BaseURL, _ = tomlString(value)
		case "env_key":
			p.EnvKey, _ = tomlString(value)
		case "experimental_bearer_token":
			p.BearerToken, _ = tomlString(value)
		case "requires_openai_auth":
			p.RequiresOpenAIAuth, _ = strconv.ParseBool(value)
		}
	}
	if !foundTable {
		return nil, fmt.Errorf("parse %s: selected Codex provider %q has no configuration table", path, providerID)
	}
	if p.BaseURL == "" {
		return nil, fmt.Errorf("parse %s: selected Codex provider %q has no base_url; refusing to guess where its credential belongs", path, providerID)
	}
	if isAgentSwapURL(p.BaseURL, "/openai") {
		return nil, nil
	}
	return p, nil
}

func codexProviderTableID(table string) (string, bool) {
	const prefix = "model_providers."
	if !strings.HasPrefix(table, prefix) {
		return "", false
	}
	id := strings.TrimSpace(strings.TrimPrefix(table, prefix))
	if unquoted, ok := tomlString(id); ok {
		return unquoted, true
	}
	return id, id != ""
}

func tomlAssignment(line string) (key, value string, ok bool) {
	i := strings.IndexByte(line, '=')
	if i < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:i])
	value = strings.TrimSpace(line[i+1:])
	return key, value, key != "" && value != ""
}

func tomlString(value string) (string, bool) {
	if unquoted, err := strconv.Unquote(value); err == nil {
		return unquoted, true
	}
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return value[1 : len(value)-1], true
	}
	return "", false
}

func stripTOMLComment(line string) string {
	var quote byte
	escaped := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if escaped {
			escaped = false
			continue
		}
		if quote == '"' && c == '\\' {
			escaped = true
			continue
		}
		if c == '\'' || c == '"' {
			if quote == 0 {
				quote = c
			} else if quote == c {
				quote = 0
			}
			continue
		}
		if c == '#' && quote == 0 {
			return line[:i]
		}
	}
	return line
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func isAgentSwapURL(raw, lanePath string) bool {
	trimmed := strings.TrimRight(strings.ToLower(strings.TrimSpace(raw)), "/")
	return (strings.HasPrefix(trimmed, "http://127.0.0.1:") ||
		strings.HasPrefix(trimmed, "http://localhost:")) && strings.HasSuffix(trimmed, lanePath)
}
