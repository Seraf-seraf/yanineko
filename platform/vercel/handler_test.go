package vercel

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestNewLoggerUsesJSONInfoLevelAndServiceName(t *testing.T) {
	var output bytes.Buffer
	logger := newLogger(&output)
	logger.Debug("hidden debug event")
	logger.Info("visible event", "phase", "bootstrap")

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("logger wrote %d records, want only the INFO record", len(lines))
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("parse JSON logger output: %v", err)
	}
	if record["level"] != "INFO" || record["msg"] != "visible event" || record["service"] != "yanineko" || record["phase"] != "bootstrap" {
		t.Fatalf("logger record = %#v, want INFO JSON with service and phase", record)
	}
	if slog.Default().Enabled(context.Background(), slog.LevelDebug) || !slog.Default().Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("process default logger does not use the INFO threshold")
	}
}
