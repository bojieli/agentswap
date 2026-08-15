package lane

import (
	"net/http"
	"testing"
	"time"
)

var now = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

func TestParseTimestamp(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
		ok   bool
	}{
		{"2026-08-15T17:00:00Z", time.Date(2026, 8, 15, 17, 0, 0, 0, time.UTC), true},
		{"2026-08-15T17:00:00+02:00", time.Date(2026, 8, 15, 15, 0, 0, 0, time.UTC), true},
		{"1786806000", time.Unix(1786806000, 0), true},         // unix seconds
		{"1786806000000", time.UnixMilli(1786806000000), true}, // unix milliseconds
		{"  1786806000  ", time.Unix(1786806000, 0), true},     // headers arrive padded
		{"1786806000.5", time.Unix(1786806000, 0), true},       // fractional seconds
		{"", time.Time{}, false},
		{"soon", time.Time{}, false},
		{"0", time.Time{}, false},
		{"-5", time.Time{}, false},
	}
	for _, c := range cases {
		got, ok := ParseTimestamp(c.in)
		if ok != c.ok {
			t.Errorf("ParseTimestamp(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && !got.Equal(c.want) {
			t.Errorf("ParseTimestamp(%q) = %v, want %v", c.in, got.UTC(), c.want.UTC())
		}
	}
}

// Getting the unit wrong turns a reset an hour away into one 55 years away, or
// the reverse — a wait that never ends, or a limit hammered on every retry.
// The rule is a magnitude threshold, so pin it from both sides.
func TestParseTimestampUnitBoundary(t *testing.T) {
	// A contemporary reset time, spelled both ways.
	inSeconds, ok := ParseTimestamp("1786806000")
	if !ok || inSeconds.Year() != 2026 {
		t.Errorf("seconds read as %v, want 2026", inSeconds.UTC())
	}
	inMillis, ok := ParseTimestamp("1786806000000")
	if !ok || inMillis.Year() != 2026 {
		t.Errorf("milliseconds read as %v, want 2026", inMillis.UTC())
	}
	if !inSeconds.Equal(inMillis) {
		t.Errorf("the same instant read as %v and %v", inSeconds.UTC(), inMillis.UTC())
	}

	// Either side of the threshold itself, which is where a misread would
	// start: 1e12 seconds is the year 33658, so anything larger is milliseconds.
	if got, _ := ParseTimestamp("999999999999"); got.Year() != 33658 {
		t.Errorf("just below the threshold read as %v, want it treated as seconds", got.UTC())
	}
	if got, _ := ParseTimestamp("1000000000001"); got.Year() != 2001 {
		t.Errorf("just above the threshold read as %v, want it treated as milliseconds", got.UTC())
	}
}

func TestParsePercent(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"0", 0, true},
		{"42", 42, true},
		{"98.6", 98.6, true},
		{"100", 100, true},
		{"150", 100, true}, // clamped rather than rejected
		{" 42 ", 42, true},
		{"", 0, false},
		{"-1", 0, false},
		{"lots", 0, false},
	}
	for _, c := range cases {
		got, ok := ParsePercent(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("ParsePercent(%q) = %v, %v; want %v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// 0.5 is ambiguous between "0.5%" and "50%". Reading it as a percent
// under-reports utilization, which costs only the predictive rotation; reading
// it as a fraction would drain a nearly untouched account.
func TestParsePercentDoesNotGuessFractions(t *testing.T) {
	got, ok := ParsePercent("0.5")
	if !ok || got != 0.5 {
		t.Errorf("ParsePercent(\"0.5\") = %v, %v; want 0.5 read as a percentage", got, ok)
	}
}

func TestRetryAfterHeader(t *testing.T) {
	cases := []struct {
		name string
		set  string
		want time.Duration
		ok   bool
	}{
		{"delay seconds", "30", 30 * time.Second, true},
		{"fractional seconds", "1.5", 1500 * time.Millisecond, true},
		{"zero", "0", 0, true},
		{"http date", now.Add(90 * time.Second).UTC().Format(http.TimeFormat), 90 * time.Second, true},
		{"http date in the past", now.Add(-time.Hour).UTC().Format(http.TimeFormat), 0, true},
		{"absent", "", 0, false},
		{"nonsense", "later", 0, false},
		{"negative", "-30", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			if c.set != "" {
				h.Set("Retry-After", c.set)
			}
			got, ok := RetryAfterHeader(h, now)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if ok && got != c.want {
				t.Errorf("= %v, want %v", got, c.want)
			}
		})
	}
}

func TestActionString(t *testing.T) {
	// These strings end up in logs, which is how a rotation decision gets
	// explained after the fact.
	cases := map[Action]string{
		ActionRelay:       "relay",
		ActionRetrySame:   "retry-same",
		ActionRotate:      "rotate",
		ActionRefreshAuth: "refresh-auth",
		ActionFatal:       "fatal",
		Action(99):        "unknown",
	}
	for a, want := range cases {
		if got := a.String(); got != want {
			t.Errorf("Action(%d).String() = %q, want %q", a, got, want)
		}
	}
}
