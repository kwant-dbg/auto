package llm

import (
	"testing"
	"time"
)

func TestResetTime(t *testing.T) {
	now := time.Date(2026, 10, 22, 21, 50, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"You've hit your session limit · resets 10:30pm (UTC)": time.Date(2026, 10, 22, 22, 31, 0, 0, time.UTC),
		"limit reached · resets 9pm (UTC)":                     time.Date(2026, 10, 23, 21, 1, 0, 0, time.UTC), // already past today
		"resets 12am (UTC)":                                    time.Date(2026, 10, 23, 0, 1, 0, 0, time.UTC),
	}
	for msg, want := range cases {
		got, ok := ResetTime(msg, now)
		if !ok || !got.Equal(want) {
			t.Errorf("ResetTime(%q) = %v, %v; want %v", msg, got, ok, want)
		}
	}
	if _, ok := ResetTime("rate limited", now); ok {
		t.Error("no reset time in the message")
	}
}
