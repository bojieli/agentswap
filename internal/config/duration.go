package config

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration wraps time.Duration so config files can spell durations the way
// humans do ("90s", "30m") instead of as nanosecond integers.
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch x := v.(type) {
	case string:
		parsed, err := time.ParseDuration(x)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", x, err)
		}
		*d = Duration(parsed)
	case float64:
		// Bare numbers are seconds. Nanoseconds would be a bizarre thing for a
		// human to type and a footgun if we guessed wrong.
		*d = Duration(time.Duration(x) * time.Second)
	default:
		return fmt.Errorf("invalid duration %v", v)
	}
	return nil
}
