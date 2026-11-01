package webhook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"net/http"
	"net/http/httptest"

	"github.com/Seraf-seraf/yanineko/internal/adapters/telegram"
	"github.com/Seraf-seraf/yanineko/internal/config"
	"github.com/Seraf-seraf/yanineko/internal/domain"
)

func TestServeHTTPMethodAndSecret(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		method     string
		secret     string
		wantStatus int
	}{
		{name: "method", method: http.MethodGet, secret: "valid_secret", wantStatus: http.StatusMethodNotAllowed},
		{name: "missing secret", method: http.MethodPost, wantStatus: http.StatusUnauthorized},
		{name: "wrong secret", method: http.MethodPost, secret: "wrong_secret", wantStatus: http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			handler := Handler{Config: testConfig()}
			request := httptest.NewRequest(test.method, "/api/webhook", strings.NewReader(`{"update_id":1,"message":{}}`))
			if test.secret != "" {
				request.Header.Set("X-Telegram-Bot-Api-Secret-Token", test.secret)
			}
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if test.method == http.MethodGet && response.Header().Get("Allow") != http.MethodPost {
				t.Fatalf("Allow = %q, want POST", response.Header().Get("Allow"))
			}
		})
	}
}

func TestServeHTTPRejectsOversizedAndMalformedBodies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{name: "malformed JSON", body: `{`, wantStatus: http.StatusBadRequest},
		{name: "multiple event fields", body: `{"update_id":1,"message":{},"edited_message":{}}`, wantStatus: http.StatusBadRequest},
		{name: "oversized body", body: fmt.Sprintf(`{"update_id":1,"event":"%s"}`, strings.Repeat("x", int(MaxBodyBytes))), wantStatus: http.StatusRequestEntityTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := authenticatedRequest(test.body)
			response := httptest.NewRecorder()
			Handler{Config: testConfig()}.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
		})
	}
}

func TestServeHTTPLogsGroupDecisionAndSanitizesProcessingFailure(t *testing.T) {
	var logOutput bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logOutput, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	ignored := Handler{Config: testConfig()}
	ignoredResponse := httptest.NewRecorder()
	ignored.ServeHTTP(ignoredResponse, authenticatedRequest(messageJSON(71, "group", "PRIVATE_MESSAGE_MARKER", "", false, false)))
	if ignoredResponse.Code != http.StatusOK {
		t.Fatalf("ignored update status = %d, want 200", ignoredResponse.Code)
	}
	if !strings.Contains(logOutput.String(), `"reason":"group_not_addressed"`) || !strings.Contains(logOutput.String(), `"chat_type":"supergroup"`) {
		t.Fatalf("logs do not explain the group classification: %s", logOutput.String())
	}

	logOutput.Reset()
	accepted := Handler{
		Config: testConfig(),
		BotUsername: func(context.Context) (string, error) {
			return "yanineko", nil
		},
		Process: func(context.Context, domain.Incoming) error { return nil },
	}
	acceptedResponse := httptest.NewRecorder()
	accepted.ServeHTTP(acceptedResponse, authenticatedRequest(messageJSON(73, "group", "@yanineko hello", "", false, false)))
	if acceptedResponse.Code != http.StatusOK || !strings.Contains(logOutput.String(), `"reason":"group_mention"`) {
		t.Fatalf("accepted group mention status/logs = %d/%s", acceptedResponse.Code, logOutput.String())
	}

	logOutput.Reset()
	failed := Handler{
		Config: testConfig(),
		Process: func(context.Context, domain.Incoming) error {
			return errors.New("PRIVATE_ERROR_MARKER")
		},
	}
	failedResponse := httptest.NewRecorder()
	failed.ServeHTTP(failedResponse, authenticatedRequest(messageJSON(72, "private", "PRIVATE_MESSAGE_MARKER", "", false, false)))
	if failedResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed update status = %d, want 503", failedResponse.Code)
	}
	logs := logOutput.String()
	if !strings.Contains(logs, `"phase":"update_processing"`) || !strings.Contains(logs, `"outcome":"failed"`) {
		t.Fatalf("logs do not report processing failure: %s", logs)
	}
	for _, forbidden := range []string{"PRIVATE_MESSAGE_MARKER", "PRIVATE_ERROR_MARKER"} {
		if strings.Contains(logs, forbidden) {
			t.Errorf("logs contain sensitive marker %q: %s", forbidden, logs)
		}
	}
}

func TestServeHTTPIgnoresUnsupportedAndUnaddressedUpdates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      string
		wantCalls int
	}{
		{name: "edited message", body: `{"update_id":1,"edited_message":{"message_id":1}}`},
		{name: "channel post", body: `{"update_id":2,"channel_post":{"message_id":1}}`},
		{name: "unaddressed group", body: messageJSON(3, "group", "hello", "", false, false)},
		{name: "lookalike bot mention", body: messageJSON(6, "group", "@yanineko_cat_bot_backup привет", "", false, false)},
		{name: "bot mention inside email", body: messageJSON(7, "group", "mail@yanineko_cat_bot", "", false, false)},
		{name: "bot message", body: messageJSON(4, "private", "hello", "", true, false)},
		{name: "private photo without caption", body: `{"update_id":5,"message":{"message_id":1,"chat":{"id":1,"type":"private"},"from":{"id":2,"is_bot":false},"photo":[{}]}}`, wantCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			handler := Handler{
				Config: testConfig(),
				BotUsername: func(context.Context) (string, error) {
					return "yanineko_cat_bot", nil
				},
				Process: func(context.Context, domain.Incoming) error {
					calls++
					return nil
				},
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, authenticatedRequest(test.body))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.Code)
			}
			if calls != test.wantCalls {
				t.Fatalf("processor calls = %d, want %d", calls, test.wantCalls)
			}
		})
	}
}

func TestServeHTTPDispatchesPrivateTextAndGroupMentionOrReply(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		body        string
		username    string
		wantProfile bool
	}{
		{name: "private text", body: messageJSON(10, "private", "hello", "", false, false)},
		{name: "group mention", body: `{"update_id":11,"message":{"message_id":10,"chat":{"id":-42,"type":"supergroup"},"from":{"id":7,"is_bot":false},"text":"😀 Привет @yanineko","entities":[{"type":"mention","offset":10,"length":9}]}}`, username: "yanineko", wantProfile: true},
		{name: "group mention without entities", body: messageJSON(13, "group", "@yanineko_cat_bot привет", "", false, false), username: "yanineko_cat_bot", wantProfile: true},
		{name: "reply to bot", body: messageJSON(12, "group", "reply text", "bot", false, true)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			profileCalls, processCalls := 0, 0
			var got domain.Incoming
			handler := Handler{
				Config: testConfig(),
				BotUsername: func(context.Context) (string, error) {
					profileCalls++
					return test.username, nil
				},
				Process: func(_ context.Context, incoming domain.Incoming) error {
					processCalls++
					got = incoming
					return nil
				},
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, authenticatedRequest(test.body))
			if response.Code != http.StatusOK || processCalls != 1 {
				t.Fatalf("status/calls = %d/%d, want 200/1", response.Code, processCalls)
			}
			if (profileCalls == 1) != test.wantProfile {
				t.Fatalf("profile calls = %d, wantProfile = %t", profileCalls, test.wantProfile)
			}
			if got.Kind != domain.MessageText {
				t.Fatalf("incoming kind = %d, want text", got.Kind)
			}
		})
	}
}

func TestServeHTTPMarksMediaLoadFailureWithoutChangingUpdateText(t *testing.T) {
	t.Parallel()

	const body = `{"update_id":24,"message":{"message_id":12,"chat":{"id":42,"type":"private"},"from":{"id":7,"is_bot":false},"caption":"Что на фото?","photo":[{}]}}`
	var incoming domain.Incoming
	handler := Handler{
		Config: testConfig(),
		LoadMedia: func(context.Context, telegram.Update) (telegram.MediaResult, error) {
			return telegram.MediaResult{}, errors.New("private media fetch error")
		},
		Process: func(_ context.Context, got domain.Incoming) error {
			incoming = got
			return nil
		},
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(body))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if incoming.Text != "Что на фото?" || !incoming.MediaUnavailable {
		t.Fatalf("incoming = %#v, want unchanged caption and transient media failure flag", incoming)
	}
}

func TestServeHTTPLogsMediaSourceWithoutLoggingMediaContent(t *testing.T) {
	var logOutput bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logOutput, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	const body = `{"update_id":25,"message":{"message_id":13,"chat":{"id":42,"type":"private"},"from":{"id":7,"is_bot":false},"caption":"caption marker","photo":[{}]}}`
	handler := Handler{
		Config: testConfig(),
		LoadMedia: func(context.Context, telegram.Update) (telegram.MediaResult, error) {
			return telegram.MediaResult{
				Images: []domain.Image{{Data: []byte("private-frame-marker"), MIMEType: "image/png"}},
				Source: telegram.MediaSourceThumbnail,
			}, nil
		},
		Process: func(context.Context, domain.Incoming) error { return nil },
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(body))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	logs := logOutput.String()
	if !strings.Contains(logs, `"phase":"telegram_media"`) ||
		!strings.Contains(logs, `"frame_count":1`) ||
		!strings.Contains(logs, `"frame_source":"thumbnail"`) {
		t.Fatalf("logs = %s, want media source and frame count", logs)
	}
	if strings.Contains(logs, "private-frame-marker") || strings.Contains(logs, "caption marker") {
		t.Fatalf("logs contain private media content: %s", logs)
	}
}

func TestServeHTTPDispatchesAboutAndIgnoresOtherBotCommand(t *testing.T) {
	t.Parallel()

	about := `{"update_id":20,"message":{"message_id":12,"chat":{"id":-42,"type":"group"},"from":{"id":7,"is_bot":false},"text":"/about@yanineko","entities":[{"type":"bot_command","offset":0,"length":15}]}}`
	otherBot := `{"update_id":21,"message":{"message_id":13,"chat":{"id":9,"type":"private"},"from":{"id":7,"is_bot":false},"text":"/about@otherbot","entities":[{"type":"bot_command","offset":0,"length":15}]}}`

	for _, test := range []struct {
		name      string
		body      string
		wantCalls int
		wantKind  domain.MessageKind
	}{
		{name: "about", body: about, wantCalls: 1, wantKind: domain.MessageAbout},
		{name: "other bot", body: otherBot},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			handler := Handler{
				Config:      testConfig(),
				BotUsername: func(context.Context) (string, error) { return "yanineko", nil },
				Process: func(_ context.Context, incoming domain.Incoming) error {
					calls++
					if incoming.Kind != test.wantKind {
						t.Fatalf("incoming kind = %d, want %d", incoming.Kind, test.wantKind)
					}
					return nil
				},
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, authenticatedRequest(test.body))
			if response.Code != http.StatusOK || calls != test.wantCalls {
				t.Fatalf("status/calls = %d/%d, want 200/%d", response.Code, calls, test.wantCalls)
			}
		})
	}
}

func TestServeHTTPReturnsRetryableErrorWhenProcessingFails(t *testing.T) {
	t.Parallel()

	handler := Handler{
		Config: testConfig(),
		Process: func(context.Context, domain.Incoming) error {
			return errors.New("internal details")
		},
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(messageJSON(30, "private", "hello", "", false, false)))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	if strings.Contains(response.Body.String(), "internal details") {
		t.Fatalf("response leaked internal error: %q", response.Body.String())
	}
}

func authenticatedRequest(body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/api/webhook", strings.NewReader(body))
	request.Header.Set("X-Telegram-Bot-Api-Secret-Token", "valid_secret")
	return request
}

func testConfig() config.Config {
	return config.Config{TelegramBotID: 123456789, TelegramWebhookSecret: "valid_secret"}
}

func messageJSON(updateID int64, chatType, text, replyType string, isBot, replyToBot bool) string {
	chatID := int64(7)
	if chatType == "group" {
		chatID = -42
		chatType = "supergroup"
	}
	reply := ""
	if replyToBot {
		reply = `,"reply_to_message":{"from":{"id":123456789,"is_bot":true}}`
	} else if replyType != "" {
		reply = `,"reply_to_message":{"from":{"id":42,"is_bot":true}}`
	}
	return fmt.Sprintf(`{"update_id":%d,"message":{"message_id":10,"chat":{"id":%d,"type":"%s"},"from":{"id":7,"is_bot":%t},"text":%q%s}}`, updateID, chatID, chatType, isBot, text, reply)
}
