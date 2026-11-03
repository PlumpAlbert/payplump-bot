package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestLoggerLevelFiltering(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  []string
	}{
		{"default", "", []string{"INFO", "WARN", "ERROR"}},
		{"whitespace", " \t\n", []string{"INFO", "WARN", "ERROR"}},
		{"debug", " \tDeBuG\n", []string{"DEBUG", "INFO", "WARN", "ERROR"}},
		{"info", " INFO ", []string{"INFO", "WARN", "ERROR"}},
		{"warn", " WaRn ", []string{"WARN", "ERROR"}},
		{"error", " ERROR\t", []string{"ERROR"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			logger, err := newLogger(&output, tc.value)
			if err != nil {
				t.Fatal(err)
			}
			logger.Debug("debug message")
			logger.Info("info message")
			logger.Warn("warn message")
			logger.Error("error message")
			var got []string
			for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
				var record struct {
					Level string `json:"level"`
					Msg   string `json:"msg"`
				}
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatalf("invalid JSON log: %v", err)
				}
				if record.Msg != strings.ToLower(record.Level)+" message" {
					t.Fatalf("unexpected log message: %q", record.Msg)
				}
				got = append(got, record.Level)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("levels = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoggerInvalidLevelRedacted(t *testing.T) {
	for _, value := range []string{"secret-token-123", " DEBUG+1 ", "warning", "trace", "0", "info\nsecret-token-123"} {
		var output bytes.Buffer
		logger, err := newLogger(&output, value)
		if err == nil || logger != nil {
			t.Fatal("invalid level accepted")
		}
		if err.Error() != "LOG_LEVEL must be debug, info, warn, or error" {
			t.Fatal("unexpected or unredacted configuration error")
		}
		if output.Len() != 0 {
			t.Fatal("invalid configuration wrote log output")
		}
	}
}
