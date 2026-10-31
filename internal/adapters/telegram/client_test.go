package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Seraf-seraf/yanineko/internal/domain"
)

const testBotToken = "123456789:synthetic_token"
const testBotID int64 = 123456789

func TestGetMeReturnsValidatedBotUsername(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/bot"+testBotToken+"/getMe" {
			t.Errorf("request = %s %s, want POST getMe", r.Method, r.URL.Path)
		}
		if r.ContentLength != 0 {
			t.Errorf("request body length = %d, want none", r.ContentLength)
		}
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":123456789,"is_bot":true,"username":"Yani_Neko_Bot"}}`)
	}))
	defer server.Close()

	client := newTestClient(server)
	username, err := client.GetMe(context.Background())
	if err != nil || username != "Yani_Neko_Bot" {
		t.Fatalf("GetMe() = (%q, %v), want username", username, err)
	}
}

func TestGetMeRejectsMismatchedBotIdentity(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":987654321,"is_bot":true,"username":"Yani_Neko_Bot"}}`)
	}))
	defer server.Close()

	_, err := newTestClient(server).GetMe(context.Background())
	if !errors.Is(err, ErrUnexpectedBotIdentity) {
		t.Fatalf("GetMe() error = %v, want identity mismatch", err)
	}
}

func TestSendUsesReplyAndForumThreadParameters(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/bot"+testBotToken+"/sendMessage" {
			t.Errorf("request = %s %s, want POST sendMessage", r.Method, r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode send payload: %v", err)
			return
		}
		var chatID, threadID, replyID int64
		var text string
		var reply replyParameters
		if err := json.Unmarshal(payload["chat_id"], &chatID); err != nil {
			t.Errorf("chat_id payload error: %v", err)
		}
		if err := json.Unmarshal(payload["message_thread_id"], &threadID); err != nil {
			t.Errorf("message_thread_id payload error: %v", err)
		}
		if err := json.Unmarshal(payload["text"], &text); err != nil {
			t.Errorf("text payload error: %v", err)
		}
		if err := json.Unmarshal(payload["reply_parameters"], &reply); err != nil {
			t.Errorf("reply_parameters payload error: %v", err)
		}
		replyID = reply.MessageID
		if chatID != -1001234567890 || threadID != 55 || replyID != 42 || !reply.AllowSendingWithoutReply || text != "Привет, мир!" {
			t.Errorf("payload values = chat:%d thread:%d reply:%d allow:%t text:%q", chatID, threadID, replyID, reply.AllowSendingWithoutReply, text)
		}
		if _, exists := payload["parse_mode"]; exists {
			t.Error("send payload contains parse_mode, want plain text")
		}
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":901}}`)
	}))
	defer server.Close()

	client := newTestClient(server)
	messageID, err := client.Send(context.Background(), domain.Outgoing{
		Key:              domain.ConversationKey{ChatID: -1001234567890, ThreadID: 55},
		ReplyToMessageID: 42,
		Text:             "Привет, мир!",
	})
	if err != nil || messageID != 901 {
		t.Fatalf("Send() = (%d, %v), want Telegram message id 901", messageID, err)
	}
}

func TestSendOmitsZeroForumThread(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode send payload: %v", err)
			return
		}
		if _, exists := payload["message_thread_id"]; exists {
			t.Error("message_thread_id is present for a non-forum conversation")
		}
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":902}}`)
	}))
	defer server.Close()

	_, err := newTestClient(server).Send(context.Background(), domain.Outgoing{
		Key:              domain.ConversationKey{ChatID: 1234},
		ReplyToMessageID: 43,
		Text:             "plain chat",
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
}

func TestSendRejectsOversizedTextBeforeRequest(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":903}}`)
	}))
	defer server.Close()

	_, err := newTestClient(server).Send(context.Background(), domain.Outgoing{
		Key:              domain.ConversationKey{ChatID: 1234},
		ReplyToMessageID: 44,
		Text:             strings.Repeat("猫", MaxMessageRunes+1),
	})
	if !errors.Is(err, ErrInvalidTelegramRequest) {
		t.Fatalf("Send() error = %v, want invalid request", err)
	}
	if requests != 0 {
		t.Fatalf("HTTP requests = %d, want no request", requests)
	}
}

func TestTelegramAPIErrorDoesNotExposeTokenOrDescription(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, `{"ok":false,"error_code":401,"description":"bad token %s"}`, testBotToken)
	}))
	defer server.Close()

	_, err := newTestClient(server).GetMe(context.Background())
	if !errors.Is(err, ErrTelegramAPI) {
		t.Fatalf("GetMe() error = %v, want Telegram API error", err)
	}
	if strings.Contains(err.Error(), testBotToken) || strings.Contains(err.Error(), "bad token") {
		t.Fatalf("GetMe() error leaked API details: %v", err)
	}
}

func TestTelegramRejectsMalformedAndOversizedResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body func(http.ResponseWriter)
	}{
		{name: "malformed", body: func(w http.ResponseWriter) { _, _ = fmt.Fprint(w, "{") }},
		{name: "oversized", body: func(w http.ResponseWriter) { _, _ = io.WriteString(w, strings.Repeat("x", int(MaxResponseBytes+1))) }},
		{name: "missing result", body: func(w http.ResponseWriter) { _, _ = fmt.Fprint(w, `{"ok":true}`) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { test.body(w) }))
			defer server.Close()
			_, err := newTestClient(server).GetMe(context.Background())
			if !errors.Is(err, ErrTelegramResponse) {
				t.Fatalf("GetMe() error = %v, want invalid-response error", err)
			}
		})
	}
}

func TestTelegramRequestCancellationIsPreserved(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := newTestClient(server).GetMe(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetMe() error = %v, want context deadline", err)
	}
	if strings.Contains(err.Error(), testBotToken) {
		t.Fatalf("GetMe() error leaked token: %v", err)
	}
}

func newTestClient(server *httptest.Server) *Client {
	return newClient(testBotToken, testBotID, server.URL, server.Client())
}
