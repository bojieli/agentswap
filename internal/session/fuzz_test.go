package session

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// FuzzSessionValidate treats the canonical form as an untrusted boundary. It
// also checks that validation is repeatable: dangling-call warnings must not
// grow every time a caller validates the same session.
func FuzzSessionValidate(f *testing.F) {
	f.Add([]byte(`{
		"source":"codex","source_id":"seed","cwd":"/tmp/project",
		"events":[
			{"kind":"message","role":"user","parts":[{"kind":"text","text":"hello"}]},
			{"kind":"message","role":"assistant","parts":[{"kind":"tool_call","call_id":"call-1","tool_name":"read","data":{"path":"x"}}]},
			{"kind":"message","role":"tool","parts":[{"kind":"tool_result","call_id":"call-1","text":"ok"}]}
		]
	}`))
	f.Add([]byte(`{
		"source":"claude","source_id":"dangling","cwd":"/tmp/project",
		"events":[{"kind":"message","role":"assistant","parts":[{"kind":"tool_call","call_id":"call-1","tool_name":"read","data":{}}]}]
	}`))
	f.Add([]byte(`{"source_id":"broken","cwd":"/tmp/project","events":[{"kind":"future"}]}`))
	f.Add([]byte("not json"))

	f.Fuzz(func(t *testing.T, data []byte) {
		var history Session
		if err := json.Unmarshal(data, &history); err != nil {
			return
		}
		first := history.Validate()
		warnings := append([]string(nil), history.Warnings...)
		second := history.Validate()
		if (first == nil) != (second == nil) {
			t.Fatalf("validation changed result: first=%v second=%v", first, second)
		}
		if first != nil && first.Error() != second.Error() {
			t.Fatalf("validation changed error: first=%q second=%q", first, second)
		}
		if strings.Join(warnings, "\x00") != strings.Join(history.Warnings, "\x00") {
			t.Fatalf("validation was not idempotent: first=%q second=%q", warnings, history.Warnings)
		}
	})
}

// FuzzReadJSONL exercises malformed, blank, multi-record, and valid JSONL
// input through the same bounded reader used by every file-backed adapter.
func FuzzReadJSONL(f *testing.F) {
	f.Add([]byte("{\"type\":\"one\"}\n{\"type\":\"two\"}\n"))
	f.Add([]byte("\n  {\"unicode\":\"世界\"}  \n\n"))
	f.Add([]byte("{\"truncated\":"))
	f.Add([]byte{0, 1, 2, '\n'})

	f.Fuzz(func(t *testing.T, data []byte) {
		path := t.TempDir() + string(os.PathSeparator) + "session.jsonl"
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		_ = readJSONL(path, func(line int, raw json.RawMessage) error {
			if line < 1 {
				t.Fatalf("invalid line number %d", line)
			}
			if !json.Valid(raw) {
				t.Fatalf("callback received invalid JSON: %q", raw)
			}
			return nil
		})
	})
}
