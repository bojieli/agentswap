// Package store holds agentswap's credential pool and its persisted health state.
//
// Credentials (accounts.json) and health (state.json) are deliberately separate
// files: accounts.json is hand-editable and never rewritten by the hot path,
// while state.json churns constantly as quota is observed.
package store

import (
	"encoding/json"
	"fmt"
	"time"
)

// LaneID names a wire protocol, not a vendor. A lane is the unit of
// interchangeability: any account in a lane can serve any request in that lane
// without translating the request body.
type LaneID string

const (
	LaneAnthropic LaneID = "anthropic"
	LaneOpenAI    LaneID = "openai"
)

func (l LaneID) Valid() bool { return l == LaneAnthropic || l == LaneOpenAI }

// Kind distinguishes a subscription login from a metered API key. It decides
// how the account is authorized upstream and how it is ordered during
// selection: subscriptions are spent first because they are already paid for.
type Kind string

const (
	KindOAuth  Kind = "oauth"
	KindAPIKey Kind = "api_key"
)

// Account is one credential in the pool.
type Account struct {
	ID       string `json:"id"`
	Lane     LaneID `json:"lane"`
	Kind     Kind   `json:"kind"`
	Label    string `json:"label"`
	Priority int    `json:"priority"` // lower is preferred
	Enabled  bool   `json:"enabled"`

	// OAuth credentials. ExpiresAt is unix milliseconds to match the on-disk
	// format both CLIs already use.
	AccessToken      string   `json:"access_token,omitempty"`
	RefreshToken     string   `json:"refresh_token,omitempty"`
	ExpiresAt        int64    `json:"expires_at,omitempty"`
	Scopes           []string `json:"scopes,omitempty"`
	SubscriptionType string   `json:"subscription_type,omitempty"`

	// ChatGPT-Account-Id, required by the Codex subscription backend. Dropping
	// it yields 401/403 even with a valid bearer token.
	ChatGPTAccountID string `json:"chatgpt_account_id,omitempty"`

	// API key credentials. BaseURL overrides the lane default and is what
	// makes same-protocol third-party providers work.
	APIKey  string `json:"api_key,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
}

// UnmarshalJSON reads an account, defaulting enabled to true when the key is
// absent.
//
// accounts.json is documented as hand-editable, and JSON's zero value for a
// bool would make an entry someone typed by hand silently inert: `list` shows
// it as disabled, `doctor` reports the lane as empty, and nothing says why.
// Disabling is the deliberate, unusual act, so it is the one that has to be
// spelled out.
func (a *Account) UnmarshalJSON(b []byte) error {
	// The alias sheds the method set, so unmarshalling does not recurse.
	type alias Account
	aux := struct {
		Enabled *bool `json:"enabled"`
		*alias
	}{alias: (*alias)(a)}

	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	a.Enabled = aux.Enabled == nil || *aux.Enabled
	return nil
}

// TokenExpired reports whether the access token is expired or close enough to
// expiry that we should refresh before spending a request on it.
func (a *Account) TokenExpired(skew time.Duration) bool {
	return a.TokenExpiredAt(time.Now(), skew)
}

// TokenExpiredAt is TokenExpired against a caller-supplied clock, so the engine
// judges expiry with the same time it uses for every other deadline.
func (a *Account) TokenExpiredAt(now time.Time, skew time.Duration) bool {
	if a.Kind != KindOAuth || a.ExpiresAt == 0 {
		return false
	}
	return time.UnixMilli(a.ExpiresAt).Add(-skew).Before(now)
}

// Clone returns a deep copy. The store hands out clones rather than pointers
// into the pool: an account is read on every request and written by token
// refresh, and sharing one struct across those goroutines is a data race.
func (a *Account) Clone() *Account {
	if a == nil {
		return nil
	}
	cp := *a
	if a.Scopes != nil {
		cp.Scopes = append([]string(nil), a.Scopes...)
	}
	return &cp
}

// SameCredentialAs reports whether two accounts are the same login.
//
// Pooling one account twice is worse than pooling it once: `status` shows two
// entries, the user believes they have failover, and both are refused in the
// same instant because they are the same account. Nothing else in the system
// can detect that, so import has to.
//
// Identity is whatever the upstream gave us that names the account. Codex
// supplies a workspace id. Anthropic supplies neither an id nor an email, so
// the tokens themselves are the only handle — which is sound, because two
// separate logins never share one.
func (a *Account) SameCredentialAs(other *Account) bool {
	if a == nil || other == nil || a.Lane != other.Lane || a.Kind != other.Kind {
		return false
	}
	switch a.Kind {
	case KindAPIKey:
		return a.APIKey != "" && a.APIKey == other.APIKey
	default:
		if a.ChatGPTAccountID != "" || other.ChatGPTAccountID != "" {
			return a.ChatGPTAccountID == other.ChatGPTAccountID
		}
		if a.RefreshToken != "" && a.RefreshToken == other.RefreshToken {
			return true
		}
		return a.AccessToken != "" && a.AccessToken == other.AccessToken
	}
}

// Problem describes why this account cannot serve a request, or "" when it
// can. It covers what a hand-edited file can get wrong about one entry, as
// opposed to what makes the whole file ambiguous.
//
// Reported rather than fatal: an account nobody can use is worth saying out
// loud, but refusing to load the pool over it would take away the commands
// needed to fix it.
func (a *Account) Problem() string {
	switch a.Kind {
	case KindAPIKey:
		if a.APIKey == "" {
			return "no api_key"
		}
	case KindOAuth:
		if a.AccessToken == "" && a.RefreshToken == "" {
			return "no token"
		}
	default:
		return fmt.Sprintf("unknown kind %q, want oauth or api_key", a.Kind)
	}
	return ""
}

// Display returns the human-facing name used in logs and `agentswap status`.
func (a *Account) Display() string {
	if a.Label != "" {
		return a.Label
	}
	return a.ID
}
