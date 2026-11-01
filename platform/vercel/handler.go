package vercel

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/Seraf-seraf/go-toolkit/pkg/logx"
	openaiadapter "github.com/Seraf-seraf/yanineko/internal/adapters/openai"
	"github.com/Seraf-seraf/yanineko/internal/adapters/storage/sqlite"
	"github.com/Seraf-seraf/yanineko/internal/adapters/telegram"
	"github.com/Seraf-seraf/yanineko/internal/adapters/webhook"
	"github.com/Seraf-seraf/yanineko/internal/app"
	"github.com/Seraf-seraf/yanineko/internal/config"
	"github.com/Seraf-seraf/yanineko/internal/domain"
	"github.com/Seraf-seraf/yanineko/internal/knowledge"
)

func init() {
	slog.SetDefault(newLogger(os.Stdout))
}

func newLogger(writer io.Writer) *slog.Logger {
	return logx.Must(logx.Config{
		Level:   "info",
		Format:  "json",
		Service: "yanineko",
		Writer:  writer,
	})
}

func Handler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		slog.ErrorContext(r.Context(), "configuration unavailable", "event", "stage_failed", "phase", "config", "code", "config_invalid")
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	telegramClient, err := telegram.NewClient(cfg.TelegramBotToken, cfg.TelegramBotID)
	if err != nil {
		slog.ErrorContext(r.Context(), "configuration unavailable", "event", "stage_failed", "phase", "config", "code", "config_invalid")
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	responder := openaiadapter.New(cfg.OpenAIAPIKey, cfg.OpenAIModel, cfg.OpenAIReasoningEffort, cfg.OpenAIBaseURL)
	memory, err := knowledge.Default()
	if err != nil {
		slog.ErrorContext(r.Context(), "world memory unavailable", "event", "stage_failed", "phase", "knowledge", "code", "knowledge_invalid")
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	handler := webhook.Handler{
		Config:      cfg,
		BotUsername: telegramClient.GetMe,
		Process: func(ctx context.Context, incoming domain.Incoming) error {
			store, err := sqlite.Open(ctx, cfg.TursoDatabaseURL, cfg.TursoAuthToken)
			if err != nil {
				slog.ErrorContext(ctx, "conversation store unavailable", "event", "stage_failed", "update_id", incoming.UpdateID, "phase", "storage_open", "code", "storage_open_failed")
				return err
			}
			defer store.Close()
			service := app.Service{
				SystemPrompt: cfg.SystemPrompt,
				Store:        store,
				Responder:    responder,
				Memory:       memory,
				Messenger:    telegramClient,
			}
			return service.Handle(ctx, incoming)
		},
	}
	handler.ServeHTTP(w, r)
}
