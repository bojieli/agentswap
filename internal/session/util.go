package session

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const maxJSONLRecord = 64 << 20

func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	if runtime.GOOS == "windows" {
		abs = strings.ToLower(abs)
	}
	return abs, nil
}

func samePath(a, b string) bool {
	ca, errA := canonicalPath(a)
	cb, errB := canonicalPath(b)
	return errA == nil && errB == nil && ca == cb
}

func homeDir() string {
	home, _ := os.UserHomeDir()
	return home
}

func envDir(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func readJSONL(path string, fn func(int, json.RawMessage) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), maxJSONLRecord)
	line := 0
	for scanner.Scan() {
		line++
		trimmed := strings.TrimSpace(scanner.Text())
		if trimmed == "" {
			continue
		}
		if !json.Valid([]byte(trimmed)) {
			return fmt.Errorf("%s:%d: invalid JSON", path, line)
		}
		if err := fn(line, json.RawMessage(append([]byte(nil), trimmed...))); err != nil {
			return fmt.Errorf("%s:%d: %w", path, line, err)
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) || strings.Contains(err.Error(), "token too long") {
			return fmt.Errorf("%s: JSONL record exceeds %d MiB safety limit", path, maxJSONLRecord>>20)
		}
		return err
	}
	return nil
}

func writeJSONLine(w io.Writer, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

func writeJSONFile(path string, value any, perm os.FileMode) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, perm)
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b), nil
}

// newUUID7 creates the time-ordered UUID form used by current Codex thread IDs.
func newUUID7(now time.Time) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	ms := uint64(now.UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b), nil
}

func formatUUID(b [16]byte) string {
	buf := make([]byte, 36)
	hex.Encode(buf[0:8], b[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], b[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], b[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], b[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], b[10:16])
	return string(buf)
}

func shortID(prefix string) (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

func timestampOr(t time.Time, fallback time.Time) time.Time {
	if t.IsZero() {
		return fallback
	}
	return t
}

func parseFlexibleTime(raw any) time.Time {
	switch v := raw.(type) {
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, v); err == nil {
				return t
			}
		}
	case float64:
		if v > 1e12 {
			return time.UnixMilli(int64(v))
		}
		return time.Unix(int64(v), int64((v-float64(int64(v)))*1e9))
	case json.Number:
		if n, err := v.Int64(); err == nil {
			if n > 1e12 {
				return time.UnixMilli(n)
			}
			return time.Unix(n, 0)
		}
	}
	return time.Time{}
}

func jsonObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || !json.Valid(raw) {
		return json.RawMessage(`{}`)
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil {
		return raw
	}
	b, _ := json.Marshal(map[string]any{"value": json.RawMessage(raw)})
	return b
}

// mediaPart converts a provider media URL into the canonical representation.
// Data URLs are kept inline so a handoff never depends on a source-local file
// or on the target having network access. Remote URLs remain URLs and are
// emitted unchanged by writers that support them.
func mediaPart(rawURL, mediaType, filename string) (Part, error) {
	rawURL = strings.TrimSpace(rawURL)
	mediaType = strings.TrimSpace(mediaType)
	if strings.HasPrefix(strings.ToLower(rawURL), "data:") {
		header, payload, ok := strings.Cut(rawURL, ",")
		if !ok {
			return Part{}, fmt.Errorf("invalid media data URL")
		}
		meta := header
		if len(meta) >= len("data:") && strings.EqualFold(meta[:len("data:")], "data:") {
			meta = meta[len("data:"):]
		}
		pieces := strings.Split(meta, ";")
		if mediaType == "" && len(pieces) > 0 && pieces[0] != "" {
			mediaType = pieces[0]
		}
		encoded := false
		for _, piece := range pieces[1:] {
			if strings.EqualFold(piece, "base64") {
				encoded = true
			}
		}
		if !encoded {
			return Part{}, fmt.Errorf("media data URL is not base64 encoded")
		}
		payload = strings.TrimSpace(payload)
		if payload == "" {
			return Part{}, fmt.Errorf("media data URL has empty payload")
		}
		if _, err := decodeMediaBase64(payload); err != nil {
			return Part{}, fmt.Errorf("invalid media data URL payload: %w", err)
		}
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		return Part{Kind: Media, MediaType: mediaType, MediaData: payload, Filename: filename}, nil
	}
	if rawURL == "" {
		return Part{}, fmt.Errorf("media has no URL")
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return Part{Kind: Media, MediaType: mediaType, MediaURL: rawURL, Filename: filename}, nil
}

func decodeMediaBase64(value string) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if decoded, err := encoding.DecodeString(value); err == nil {
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("invalid base64 payload")
}

func mediaPartFromValue(value any, mediaType, filename string) (Part, error) {
	if obj, ok := value.(map[string]any); ok {
		if mediaType == "" {
			mediaType = stringValue(obj["media_type"])
			if mediaType == "" {
				mediaType = stringValue(obj["mime"])
			}
			if mediaType == "" {
				mediaType = stringValue(obj["mediaType"])
			}
			if mediaType == "" {
				mediaType = stringValue(obj["mimeType"])
			}
		}
		if filename == "" {
			filename = stringValue(obj["filename"])
		}
		for _, key := range []string{"url", "image_url", "file_url", "file_data", "image_data", "image", "data"} {
			if raw := stringValue(obj[key]); raw != "" {
				return mediaPart(raw, mediaType, filename)
			}
			if nested, ok := obj[key].(map[string]any); ok {
				return mediaPartFromValue(nested, mediaType, filename)
			}
		}
	}
	return mediaPart(stringValue(value), mediaType, filename)
}

func mediaDataURL(part Part) string {
	if part.MediaData != "" {
		mediaType := part.MediaType
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		return "data:" + mediaType + ";base64," + part.MediaData
	}
	return part.MediaURL
}

func hash12(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

func hashHex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func safeTitle(value, fallback string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\x00", ""))
	if value == "" {
		return fallback
	}
	runes := []rune(value)
	if len(runes) > 200 {
		return string(runes[:200])
	}
	return value
}

func firstText(s *Session) string {
	for _, event := range s.Events {
		if event.Kind != Message || event.Role != "user" {
			continue
		}
		for _, part := range event.Parts {
			if part.Kind == Text && strings.TrimSpace(part.Text) != "" {
				return safeTitle(part.Text, s.SourceID)
			}
		}
	}
	return s.SourceID
}

func proposedPlan(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	lower := strings.ToLower(trimmed)
	const open = "<proposed_plan>"
	const close = "</proposed_plan>"
	if !strings.HasPrefix(lower, open) || !strings.HasSuffix(lower, close) {
		return "", false
	}
	start := len(open)
	end := len(trimmed) - len(close)
	if end < start {
		return "", false
	}
	return strings.TrimSpace(trimmed[start:end]), true
}

func ensureDir(path string) error { return os.MkdirAll(path, 0o700) }

func removeIfExists(path string) { _ = os.RemoveAll(path) }
