package lane

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

func parseFloat(s string) (float64, error) {
	return strconv.ParseFloat(strings.TrimSpace(s), 64)
}

// ParseTimestamp accepts the formats a rate-limit reset header realistically
// arrives in: RFC 3339, unix seconds, and unix milliseconds. Being liberal here
// is deliberate — misreading a reset time either wastes a full quota window or
// hammers a limit that has not lifted.
func ParseTimestamp(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	n, err := parseFloat(s)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	// Values this large are milliseconds; a seconds value that big would be
	// in the year 33658.
	if n > 1e12 {
		return time.UnixMilli(int64(n)), true
	}
	return time.Unix(int64(n), 0), true
}

// ParsePercent reads a utilization value expressed as a percentage in [0,100].
//
// It deliberately does not try to auto-detect a 0-1 fraction: 0.5 is genuinely
// ambiguous between "0.5%" and "50%", and guessing wrong in the high direction
// would drain a perfectly good account. Reading a fraction as a percent instead
// under-reports utilization, which costs us only the predictive rotation and
// falls back to reacting to the 429 — the safe direction to be wrong in. Lanes
// whose upstream reports fractions must scale explicitly.
func ParsePercent(s string) (float64, bool) {
	v, err := parseFloat(s)
	if err != nil || v < 0 {
		return 0, false
	}
	if v > 100 {
		v = 100
	}
	return v, true
}

// InBandError is a terminal failure delivered inside a response whose status
// line claimed success: a JSON error envelope on a 200, or a standard
// terminal stream event — "error" or "response.failed" — at the head of an
// event stream. Both Claude Code and Codex understand these natively, which
// is exactly why a gateway can rely on them instead of a status code.
type InBandError struct {
	Type    string
	Code    string
	Message string

	// ResetsInSeconds is how long the upstream says the failed limit takes to
	// refill, when it says at all.
	ResetsInSeconds float64
}

// ParseInBandError scans the head of a response body for such a failure. It
// reports failures only: an ordinary opening event, a healthy JSON response,
// and anything unrecognized all yield ok == false, because mistaking content
// for an error would corrupt the relay.
func ParseInBandError(head []byte) (InBandError, bool) {
	head = bytes.TrimSpace(head)
	if len(head) == 0 {
		return InBandError{}, false
	}
	if head[0] == '{' {
		return jsonInBandError(head)
	}
	return sseInBandError(head)
}

// errorEnvelope is the standard error shape both protocols share.
type errorEnvelope struct {
	Type  string `json:"type"`
	Error *struct {
		Type             string  `json:"type"`
		Code             string  `json:"code"`
		Message          string  `json:"message"`
		ResetsInSeconds  float64 `json:"resets_in_seconds"`
		ResetAfterSecond float64 `json:"reset_after_seconds"`
	} `json:"error"`
}

func envelopeError(e *errorEnvelope) (InBandError, bool) {
	if e.Error == nil {
		return InBandError{}, false
	}
	resets := e.Error.ResetsInSeconds
	if resets <= 0 {
		resets = e.Error.ResetAfterSecond
	}
	return InBandError{
		Type: e.Error.Type, Code: e.Error.Code, Message: e.Error.Message,
		ResetsInSeconds: resets,
	}, true
}

func jsonInBandError(head []byte) (InBandError, bool) {
	// A truncated head fails to unmarshal and yields ok == false — fail-open,
	// since a healthy non-streaming response is larger than the sample.
	var d errorEnvelope
	if err := json.Unmarshal(head, &d); err != nil {
		return InBandError{}, false
	}
	return envelopeError(&d)
}

func sseInBandError(head []byte) (InBandError, bool) {
	for _, block := range strings.Split(string(head), "\n\n") {
		name, data := sseEvent(block)
		if name != "error" && name != "response.failed" {
			continue
		}
		// The common case: the event's data is itself a standard error
		// envelope, whichever protocol family the gateway grew up with.
		if f, ok := jsonInBandError(bytes.TrimSpace([]byte(data))); ok {
			return f, true
		}
		// The official response.failed nests the error inside the response
		// object instead.
		var d struct {
			Response *struct {
				Error *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &d); err == nil &&
			d.Response != nil && d.Response.Error != nil {
			return InBandError{
				Type: name, Code: d.Response.Error.Code, Message: d.Response.Error.Message,
			}, true
		}
		// A terminal event whose payload we cannot read is still terminal.
		return InBandError{Type: name}, true
	}
	return InBandError{}, false
}

// sseEvent splits one event block into its name and its joined data payload.
func sseEvent(block string) (name, data string) {
	var b strings.Builder
	for _, line := range strings.Split(block, "\n") {
		if rest, ok := strings.CutPrefix(line, "event:"); ok {
			name = strings.TrimSpace(rest)
			continue
		}
		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(strings.TrimSpace(rest))
		}
	}
	return name, b.String()
}
