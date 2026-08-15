package lane

import (
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
