package observability

import (
	"context"
	"log/slog"
	"time"
)

type Stage struct {
	updateID int64
	phase    string
	started  time.Time
}

func Start(ctx context.Context, updateID int64, phase string) Stage {
	started := time.Now()
	slog.DebugContext(ctx, "processing stage started",
		"event", "stage_started",
		"update_id", updateID,
		"phase", phase,
	)
	return Stage{updateID: updateID, phase: phase, started: started}
}

func (s Stage) Complete(ctx context.Context, fields ...any) {
	s.log(ctx, slog.LevelInfo, "processing stage completed", "stage_completed", "success", fields...)
}

func (s Stage) Outcome(ctx context.Context, outcome string, fields ...any) {
	s.log(ctx, slog.LevelInfo, "processing stage completed", "stage_completed", outcome, fields...)
}

func (s Stage) Warn(ctx context.Context, code string, fields ...any) {
	s.log(ctx, slog.LevelWarn, "processing stage warning", "stage_warning", "warning", append([]any{"code", code}, fields...)...)
}

func (s Stage) Fail(ctx context.Context, code string, fields ...any) {
	s.log(ctx, slog.LevelError, "processing stage failed", "stage_failed", "failed", append([]any{"code", code}, fields...)...)
}

func (s Stage) log(ctx context.Context, level slog.Level, message string, event string, outcome string, fields ...any) {
	attributes := make([]any, 0, 10+len(fields))
	attributes = append(attributes,
		"event", event,
		"update_id", s.updateID,
		"phase", s.phase,
		"outcome", outcome,
		"duration_ms", time.Since(s.started).Milliseconds(),
	)
	attributes = append(attributes, fields...)
	slog.Log(ctx, level, message, attributes...)
}
