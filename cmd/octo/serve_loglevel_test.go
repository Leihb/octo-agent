package main

import (
	"log/slog"
	"testing"
)

func TestServeLogLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"":        slog.LevelInfo,
		"info":    slog.LevelInfo,
		"DEBUG":   slog.LevelDebug,
		" warn ":  slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"bogus":   slog.LevelInfo,
	}
	for in, want := range cases {
		t.Setenv("OCTO_LOG_LEVEL", in)
		if got := serveLogLevel(); got != want {
			t.Errorf("serveLogLevel(%q) = %v, want %v", in, got, want)
		}
	}
}
