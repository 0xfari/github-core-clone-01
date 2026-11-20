// util.go — small shared helpers: env access, trial-time parsing, base64.

package main

import (
	"encoding/base64"
	"os"
	"time"
)

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseTrialNow parses MAKEFUN_TRIAL_NOW the way Date.parse does for the
// ISO forms the suite emits, returning the epoch seconds and the ISO string
// Date.toISOString() would produce (millis, Z). ok is false when the value
// does not parse, and the caller falls back to the fixed default.
func parseTrialNow(raw string) (epoch int64, iso string, ok bool) {
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05Z07:00",
	}
	for _, layout := range layouts {
		t, err := time.Parse(layout, raw)
		if err == nil {
			utc := t.UTC()
			return utc.Unix(), utc.Format("2006-01-02T15:04:05.000Z07:00"), true
		}
	}
	return 0, "", false
}

func b64enc(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func b64dec(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
