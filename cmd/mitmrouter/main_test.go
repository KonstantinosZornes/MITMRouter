package main

import (
	"log/slog"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	cases := []struct {
		raw  string
		want slog.Level
		ok   bool
	}{
		{raw: "debug", want: slog.LevelDebug, ok: true},
		{raw: "INFO", want: slog.LevelInfo, ok: true},
		{raw: "warning", want: slog.LevelWarn, ok: true},
		{raw: "error", want: slog.LevelError, ok: true},
		{raw: "verbose", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := parseLogLevel(tc.raw)
			if (err == nil) != tc.ok {
				t.Fatalf("parseLogLevel(%q) error = %v, want success=%v", tc.raw, err, tc.ok)
			}
			if err == nil && got != tc.want {
				t.Fatalf("parseLogLevel(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
