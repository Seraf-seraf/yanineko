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
	"github.com/Seraf-seraf/yanineko/internal/observability"
)

const MaxBodyBytes int64 = 1 << 20
const RequestTimeout = 48 * time.Second

type Handler struct {
	Config      config.Config
	BotUsername func(context.Context) (string, error)
	LoadMedia   func(context.Context, telegram.Update) (telegram.MediaResult, error)
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
		code := "invalid_update"
		if errors.As(err, &limit) {
			code = "update_too_large"
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "invalid update", http.StatusBadRequest)
		}
		slog.WarnContext(ctx, "webhook update rejected", "event", "webhook_rejected", "phase", "telegram_parse", "code", code)
		return
	}
	classification := observability.Start(ctx, update.ID, "telegram_classification")
	if !supported {
		classification.Outcome(ctx, "ignored", "reason", "unsupported_update")
		w.WriteHeader(http.StatusOK)
		return
	}

	username := ""
	if telegram.NeedsBotUsername(update) {
		if h.BotUsername == nil {
			observability.Start(ctx, update.ID, "telegram_profile").Fail(ctx, "profile_lookup_unavailable")
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		profileStage := observability.Start(ctx, update.ID, "telegram_profile")
		username, err = h.BotUsername(ctx)
		if err != nil {
			profileStage.Fail(ctx, "profile_unavailable")
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		profileStage.Complete(ctx)
	}

	incoming, accepted, reason := telegram.ClassifyWithReason(update, h.Config.TelegramBotID, username)
	classificationFields := []any{"reason", reason, "chat_type", update.Message.Chat.Type}
	if !accepted {
		classification.Outcome(ctx, "ignored", classificationFields...)
		w.WriteHeader(http.StatusOK)
		return
	}
	classification.Complete(ctx, append(classificationFields, "message_kind", incoming.Kind)...)
	if h.LoadMedia != nil {
		mediaStage := observability.Start(ctx, update.ID, "telegram_media")
		media, mediaErr := h.LoadMedia(ctx, update)
		if mediaErr != nil {
			if errors.Is(mediaErr, context.Canceled) || errors.Is(mediaErr, context.DeadlineExceeded) {
				mediaStage.Fail(ctx, "media_load_cancelled")
				http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			mediaCode := "media_load_failed"
			if errors.Is(mediaErr, telegram.ErrMediaDecoderUnavailable) {
				mediaCode = "media_decoder_unavailable"
			}
			mediaStage.Outcome(ctx, "unavailable", "code", mediaCode)
			incoming.MediaUnavailable = true
		} else {
			mediaSource := media.Source
			if mediaSource == "" {
				mediaSource = "none"
			}
			mediaStage.Complete(ctx, "frame_count", len(media.Images), "frame_source", mediaSource)
			incoming.Images = media.Images
		}
	}
	if h.Process == nil {
		observability.Start(ctx, update.ID, "update_processing").Fail(ctx, "processor_unavailable")
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	processing := observability.Start(ctx, update.ID, "update_processing")
	if err := h.Process(ctx, incoming); err != nil {
		processing.Outcome(ctx, "failed", "http_status", http.StatusServiceUnavailable)
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	processing.Complete(ctx, "http_status", http.StatusOK)
	w.WriteHeader(http.StatusOK)
}

func matchesSecret(expected, received string) bool {
	return len(expected) > 0 && len(expected) == len(received) && subtle.ConstantTimeCompare([]byte(expected), []byte(received)) == 1
}
