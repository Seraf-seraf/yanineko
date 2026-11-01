package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Seraf-seraf/yanineko/internal/adapters/telegram"
	"github.com/Seraf-seraf/yanineko/internal/adapters/webhook"
	"github.com/Seraf-seraf/yanineko/internal/app"
	"github.com/Seraf-seraf/yanineko/internal/config"
	"github.com/Seraf-seraf/yanineko/internal/domain"
)

const integrationBotID int64 = 123456789
const integrationWebhookSecret = "local_integration_secret"

func TestWebhookToSQLiteServiceEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	db, err := sql.Open("turso", ":memory:")
	if err != nil {
		t.Fatalf("open local SQLite-compatible database: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Fatalf("ping local SQLite-compatible database: %v", err)
	}
	store := newStore(db)
	t.Cleanup(func() { _ = store.Close() })
	if err := store.migrate(ctx); err != nil {
		t.Fatalf("migrate local SQLite-compatible database: %v", err)
	}

	responder := &integrationResponder{}
	messenger := &integrationMessenger{}
	botUsernameCalls := 0
	service := app.Service{
		SystemPrompt:   "test character prompt",
		Store:          store,
		Responder:      responder,
		MediaDescriber: integrationMediaDescriber{},
		Messenger:      messenger,
	}
	handler := webhook.Handler{
		Config: config.Config{
			TelegramBotID:         integrationBotID,
			TelegramWebhookSecret: integrationWebhookSecret,
		},
		BotUsername: func(context.Context) (string, error) {
			botUsernameCalls++
			return "YaniNeko", nil
		},
		LoadMedia: func(_ context.Context, update telegram.Update) ([]domain.Image, error) {
			if update.Message != nil && len(update.Message.Photo) > 0 {
				return []domain.Image{{Data: []byte("transient-image-marker"), MIMEType: "image/png"}}, nil
			}
			return nil, nil
		},
		Process: func(ctx context.Context, incoming domain.Incoming) error {
			return service.Handle(ctx, incoming)
		},
	}

	invalidSecret := serveIntegrationUpdate(t, handler, "wrong_secret", integrationUpdate{
		UpdateID: 100,
		Message:  integrationMessage(1, telegram.Chat{ID: 101, Type: "private"}, "hello", nil, nil, 0),
	})
	if invalidSecret.Code != http.StatusUnauthorized || len(responder.calls) != 0 || len(messenger.sent) != 0 {
		t.Fatalf("invalid-secret request = (%d, AI calls %d, sends %d), want 401 with no side effects", invalidSecret.Code, len(responder.calls), len(messenger.sent))
	}

	private := integrationUpdate{
		UpdateID: 100,
		Message:  integrationMessage(1, telegram.Chat{ID: 101, Type: "private"}, "hello", nil, nil, 0),
	}
	if response := serveIntegrationUpdate(t, handler, integrationWebhookSecret, private); response.Code != http.StatusOK {
		t.Fatalf("private update status = %d, want 200", response.Code)
	}
	if len(responder.calls) != 1 || len(messenger.sent) != 1 || messenger.sent[0].Text != "reply-1" {
		t.Fatalf("private path = (AI calls %d, sends %#v), want one generated reply", len(responder.calls), messenger.sent)
	}
	if responder.calls[0].prompt != "test character prompt" {
		t.Fatalf("model prompt = %q, want configured system prompt", responder.calls[0].prompt)
	}

	groupMention := integrationUpdate{
		UpdateID: 101,
		Message: integrationMessage(
			2,
			telegram.Chat{ID: -1001, Type: "supergroup"},
			"Hey @yanineko",
			[]telegram.Entity{{Type: "mention", Offset: 4, Length: 9}},
			nil,
			0,
		),
	}
	if response := serveIntegrationUpdate(t, handler, integrationWebhookSecret, groupMention); response.Code != http.StatusOK {
		t.Fatalf("group mention status = %d, want 200", response.Code)
	}
	if len(responder.calls) != 2 || len(messenger.sent) != 2 || botUsernameCalls != 1 {
		t.Fatalf("group mention path = (AI calls %d, sends %d, username lookups %d), want (2, 2, 1)", len(responder.calls), len(messenger.sent), botUsernameCalls)
	}

	groupReply := integrationUpdate{
		UpdateID: 102,
		Message: integrationMessage(
			3,
			telegram.Chat{ID: -1001, Type: "supergroup"},
			"follow up",
			nil,
			&telegram.ReplyToMessage{From: &telegram.User{ID: integrationBotID, IsBot: true}},
			7,
		),
	}
	if response := serveIntegrationUpdate(t, handler, integrationWebhookSecret, groupReply); response.Code != http.StatusOK {
		t.Fatalf("group reply status = %d, want 200", response.Code)
	}
	if len(responder.calls) != 3 || len(messenger.sent) != 3 {
		t.Fatalf("group reply path = (AI calls %d, sends %d), want (3, 3)", len(responder.calls), len(messenger.sent))
	}
	if messenger.sent[2].Key != (domain.ConversationKey{ChatID: -1001, ThreadID: 7}) || messenger.sent[2].ReplyToMessageID != 3 {
		t.Fatalf("group response target = %#v, want same forum topic and source message", messenger.sent[2])
	}

	about := integrationUpdate{
		UpdateID: 103,
		Message: integrationMessage(
			4,
			telegram.Chat{ID: 101, Type: "private"},
			"/about",
			[]telegram.Entity{{Type: "bot_command", Offset: 0, Length: 6}},
			nil,
			0,
		),
	}
	if response := serveIntegrationUpdate(t, handler, integrationWebhookSecret, about); response.Code != http.StatusOK {
		t.Fatalf("about status = %d, want 200", response.Code)
	}
	if len(responder.calls) != 3 || len(messenger.sent) != 4 || messenger.sent[3].Text != app.AboutMessage {
		t.Fatalf("about path = (AI calls %d, sends %#v), want fixed response and no model call", len(responder.calls), messenger.sent)
	}

	if response := serveIntegrationUpdate(t, handler, integrationWebhookSecret, private); response.Code != http.StatusOK {
		t.Fatalf("duplicate update status = %d, want 200", response.Code)
	}
	if len(responder.calls) != 3 || len(messenger.sent) != 4 {
		t.Fatalf("duplicate update caused side effects: AI calls %d, sends %d", len(responder.calls), len(messenger.sent))
	}

	otherChat := integrationUpdate{
		UpdateID: 104,
		Message:  integrationMessage(5, telegram.Chat{ID: 202, Type: "private"}, "separate chat", nil, nil, 0),
	}
	if response := serveIntegrationUpdate(t, handler, integrationWebhookSecret, otherChat); response.Code != http.StatusOK {
		t.Fatalf("second-chat update status = %d, want 200", response.Code)
	}
	if history := responder.calls[len(responder.calls)-1].history; len(history) != 1 || history[0].Role != domain.RoleUser || history[0].Text != "separate chat" {
		t.Fatalf("second-chat model history = %#v, want only the current chat's input", history)
	}

	sameChatAgain := integrationUpdate{
		UpdateID: 105,
		Message:  integrationMessage(6, telegram.Chat{ID: 101, Type: "private"}, "continue", nil, nil, 0),
	}
	if response := serveIntegrationUpdate(t, handler, integrationWebhookSecret, sameChatAgain); response.Code != http.StatusOK {
		t.Fatalf("same-chat continuation status = %d, want 200", response.Code)
	}
	lastHistory := responder.calls[len(responder.calls)-1].history
	if len(lastHistory) != 1 || lastHistory[0].Role != domain.RoleUser || lastHistory[0].Text != "continue" {
		t.Fatalf("same-chat incremental model history = %#v, want only input after prior checkpoint", lastHistory)
	}
	var archiveCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM conversation_messages WHERE chat_id = 101 AND thread_id = 0`).Scan(&archiveCount); err != nil || archiveCount != 4 {
		t.Fatalf("same-chat archived messages = (%d, %v), want both full turns retained", archiveCount, err)
	}

	mediaUpdate := integrationUpdate{UpdateID: 106, Message: telegram.Message{
		ID: 7, Chat: telegram.Chat{ID: 203, Type: "private"}, From: &telegram.User{ID: 707},
		Caption: "Что изображено?", Photo: []telegram.PhotoSize{{FileID: "test-photo", Width: 100, Height: 100}},
	}}
	if response := serveIntegrationUpdate(t, handler, integrationWebhookSecret, mediaUpdate); response.Code != http.StatusOK {
		t.Fatalf("media update status = %d, want 200", response.Code)
	}
	mediaHistory := responder.calls[len(responder.calls)-1].history
	wantDescription := "Что изображено?\n\n[Описание изображения: На изображении чёрная кошка.]"
	if len(mediaHistory) != 1 || mediaHistory[0].Text != wantDescription {
		t.Fatalf("media model history = %#v, want textual visual description", mediaHistory)
	}
	var storedInput, archivedText string
	if err := db.QueryRowContext(ctx, `SELECT input_text FROM processed_updates WHERE update_id = 106`).Scan(&storedInput); err != nil || storedInput != mediaUpdate.Message.Caption {
		t.Fatalf("idempotency input = (%q, %v), want original caption", storedInput, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT content FROM conversation_messages WHERE update_id = 106 AND role = 'user'`).Scan(&archivedText); err != nil || archivedText != wantDescription {
		t.Fatalf("archived user input = (%q, %v), want image description", archivedText, err)
	}
	if strings.Contains(archivedText, "transient-image-marker") {
		t.Fatal("archived user input contains image bytes")
	}
}

type integrationUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  telegram.Message `json:"message"`
}

type integrationResponseCall struct {
	prompt  string
	history []domain.Message
}

type integrationResponder struct {
	calls []integrationResponseCall
}

type integrationMediaDescriber struct{}

func (integrationMediaDescriber) Describe(_ context.Context, _ int64, _ []domain.Image) (string, error) {
	return "На изображении чёрная кошка.", nil
}

func (r *integrationResponder) Respond(ctx context.Context, request domain.AgentRequest, _ domain.AgentMemory) (domain.AgentResult, error) {
	if err := ctx.Err(); err != nil {
		return domain.AgentResult{}, err
	}
	r.calls = append(r.calls, integrationResponseCall{
		prompt:  request.Instructions,
		history: append([]domain.Message(nil), request.Messages...),
	})
	return domain.AgentResult{Text: "reply-" + strconv.Itoa(len(r.calls)), ItemsJSON: []byte("[]")}, nil
}

type integrationMessenger struct {
	sent []domain.Outgoing
}

func (m *integrationMessenger) Send(ctx context.Context, outgoing domain.Outgoing) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	m.sent = append(m.sent, outgoing)
	return int64(900 + len(m.sent)), nil
}

func serveIntegrationUpdate(t *testing.T, handler webhook.Handler, secret string, update integrationUpdate) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(update)
	if err != nil {
		t.Fatalf("marshal update: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/webhook", bytes.NewReader(body))
	request.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func integrationMessage(messageID int64, chat telegram.Chat, text string, entities []telegram.Entity, reply *telegram.ReplyToMessage, threadID int64) telegram.Message {
	return telegram.Message{
		ID:             messageID,
		Chat:           chat,
		From:           &telegram.User{ID: 700 + messageID},
		Text:           text,
		Entities:       entities,
		ReplyToMessage: reply,
		ThreadID:       threadID,
	}
}
