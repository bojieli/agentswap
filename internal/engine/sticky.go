package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// conversationPrefix is how much of a request body is hashed to recognise a
// continuing conversation. A conversation's opening bytes — model, system
// prompt, first messages — stay byte-identical as it grows, so a hash of the
// prefix is stable across turns. It is also very nearly the prompt-cache key,
// which is the point: keeping a conversation on one account is what keeps its
// cache warm.
const conversationPrefix = 4096

// ConversationKey fingerprints a request body so successive turns of the same
// conversation route to the same account.
func ConversationKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if len(body) > conversationPrefix {
		body = body[:conversationPrefix]
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:16])
}

// stickyMap remembers which account last served a conversation.
type stickyMap struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]stickyEntry
}

type stickyEntry struct {
	accountID string
	at        time.Time
}

func newStickyMap(ttl time.Duration) *stickyMap {
	return &stickyMap{ttl: ttl, m: map[string]stickyEntry{}}
}

func (s *stickyMap) get(key string, now time.Time) (string, bool) {
	if key == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key]
	if !ok || now.Sub(e.at) > s.ttl {
		return "", false
	}
	return e.accountID, true
}

func (s *stickyMap) set(key, accountID string, now time.Time) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = stickyEntry{accountID: accountID, at: now}

	// Opportunistic sweep. Without it a long-lived daemon accumulates one
	// entry per conversation it has ever seen.
	if len(s.m) > 4096 {
		for k, v := range s.m {
			if now.Sub(v.at) > s.ttl {
				delete(s.m, k)
			}
		}
	}
}
