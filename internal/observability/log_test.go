package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestStageEventsUseStandardLevelsAndSafeFields(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	stage := Start(context.Background(), 901, "memory_search")
	stage.Complete(context.Background(), "result_count", 2, "fact_ids", []string{"anime.aya", "manga.yaniko"})
	Start(context.Background(), 901, "telegram_send").Fail(context.Background(), "telegram_send_failed")

	lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'})
	if len(lines) != 4 {
		t.Fatalf("log records = %d, want two starts, one completion, and one failure", len(lines))
	}
	var events []map[string]any
	for _, line := range lines {
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("parse JSON log: %v", err)
		}
		events = append(events, event)
	}
	if events[0]["level"] != "DEBUG" || events[0]["event"] != "stage_started" {
		t.Errorf("start event = %#v, want DEBUG stage_started", events[0])
	}
	if events[1]["level"] != "INFO" || events[1]["event"] != "stage_completed" || events[1]["outcome"] != "success" {
		t.Errorf("completion event = %#v, want INFO successful stage_completed", events[1])
	}
	if events[1]["update_id"] != float64(901) || events[1]["phase"] != "memory_search" || events[1]["result_count"] != float64(2) {
		t.Errorf("completion fields = %#v, want update correlation, phase, and result count", events[1])
	}
	if events[1]["duration_ms"] == nil || events[1]["fact_ids"] == nil {
		t.Errorf("completion is missing duration or fact IDs: %#v", events[1])
	}
	if events[3]["level"] != "ERROR" || events[3]["event"] != "stage_failed" || events[3]["code"] != "telegram_send_failed" {
		t.Errorf("failure event = %#v, want ERROR with bounded code", events[3])
	}
}

func TestInfoThresholdSuppressesDebugStageStart(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	Start(context.Background(), 902, "telegram_send").Complete(context.Background())
	lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'})
	if len(lines) != 1 {
		t.Fatalf("INFO threshold wrote %d records, want only completion", len(lines))
	}
	var event map[string]any
	if err := json.Unmarshal(lines[0], &event); err != nil {
		t.Fatalf("parse JSON log: %v", err)
	}
	if event["level"] != "INFO" || event["event"] != "stage_completed" {
		t.Fatalf("visible event = %#v, want INFO stage_completed", event)
	}
}
