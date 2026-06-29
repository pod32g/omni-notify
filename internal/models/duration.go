package models

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration wraps time.Duration so it serialises to/from human-friendly strings
// like "5m" or "30s" in both JSON and YAML, instead of raw nanoseconds.
//
// A JSON number is also accepted and interpreted as seconds, which keeps simple
// API clients happy.
type Duration time.Duration

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// String renders the duration using time.Duration's formatting.
func (d Duration) String() string { return time.Duration(d).String() }

// MarshalJSON renders the duration as a string ("5m0s"), or "" when zero.
func (d Duration) MarshalJSON() ([]byte, error) {
	if d == 0 {
		return []byte(`""`), nil
	}
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts either a duration string ("5m") or a number of seconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	return d.fromAny(v)
}

// UnmarshalYAML accepts either a duration string ("5m") or a number of seconds.
func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var v any
	if err := unmarshal(&v); err != nil {
		return err
	}
	return d.fromAny(v)
}

func (d *Duration) fromAny(v any) error {
	switch val := v.(type) {
	case nil:
		*d = 0
	case string:
		if val == "" {
			*d = 0
			return nil
		}
		parsed, err := time.ParseDuration(val)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", val, err)
		}
		*d = Duration(parsed)
	case float64:
		*d = Duration(time.Duration(val) * time.Second)
	case int:
		*d = Duration(time.Duration(val) * time.Second)
	case int64:
		*d = Duration(time.Duration(val) * time.Second)
	default:
		return fmt.Errorf("invalid duration value of type %T", v)
	}
	return nil
}
