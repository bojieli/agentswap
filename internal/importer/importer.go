// Package importer reads credentials already on disk from a normal `claude`
// or `codex` login, so adopting agentswap does not mean logging in again.
package importer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

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
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, err
	}
	return filepath.Join(home, ".claude", ".credentials.json"), false, nil
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

// ImportCodex builds an account from the current Codex login, preferring the
// ChatGPT subscription over a bare API key when both are present.
func ImportCodex() (*store.Account, error) {
	path, err := CodexAuthPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w at %s; run `codex login` first", ErrNoCredentials, path)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var a codexAuth
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("parse codex auth: %w", err)
	}

	if a.Tokens.AccessToken != "" {
		return &store.Account{
			Lane: store.LaneOpenAI, Kind: store.KindOAuth, Enabled: true,
			AccessToken:      a.Tokens.AccessToken,
			RefreshToken:     a.Tokens.RefreshToken,
			ChatGPTAccountID: a.Tokens.AccountID,
		}, nil
	}
	if a.APIKey != "" {
		return &store.Account{
			Lane: store.LaneOpenAI, Kind: store.KindAPIKey, Enabled: true,
			APIKey: a.APIKey,
		}, nil
	}
	return nil, fmt.Errorf("%w: codex auth holds neither tokens nor an api key", ErrNoCredentials)
}
