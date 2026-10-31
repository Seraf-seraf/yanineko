package webhook

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Seraf-seraf/yanineko/internal/adapters/telegram"
	"github.com/Seraf-seraf/yanineko/internal/config"
	"github.com/Seraf-seraf/yanineko/internal/domain"
)

const MaxBodyBytes int64 = 1 << 20
const RequestTimeout = 48 * time.Second

type Handler struct {
	Config      config.Config
	BotUsername func(context.Context) (string, error)
	Process     func(context.Context, domain.Incoming) error
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !matchesSecret(h.Config.TelegramWebhookSecret, r.Header.Get("X-Telegram-Bot-Api-Secret-Token")) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), RequestTimeout)
	defer cancel()
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	defer r.Body.Close()
	update, supported, err := telegram.ParseUpdate(r.Body)
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "invalid update", http.StatusBadRequest)
		}
		return
	}
	if !supported {
		w.WriteHeader(http.StatusOK)
		return
	}

	username := ""
	if telegram.NeedsBotUsername(update) {
		if h.BotUsername == nil {
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		username, err = h.BotUsername(ctx)
		if err != nil {
			slog.ErrorContext(ctx, "bot identity unavailable", "update_id", update.ID, "phase", "telegram_profile", "code", "profile_unavailable")
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
	}

	incoming, accepted := telegram.Classify(update, h.Config.TelegramBotID, username)
	if !accepted {
		w.WriteHeader(http.StatusOK)
		return
	}
	if h.Process == nil {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := h.Process(ctx, incoming); err != nil {
		slog.ErrorContext(ctx, "update processing failed", "update_id", update.ID, "phase", "process", "code", "processing_failed")
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func matchesSecret(expected, received string) bool {
	return len(expected) > 0 && len(expected) == len(received) && subtle.ConstantTimeCompare([]byte(expected), []byte(received)) == 1
}
