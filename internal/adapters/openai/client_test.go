package openaiadapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Seraf-seraf/yanineko/internal/domain"
)

func TestRespondUsesResponsesAPIInstructionsHistoryAndNoServerStorage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/responses") {
			t.Errorf("request = %s %s, want POST /responses", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer synthetic-openai-key" {
			t.Errorf("Authorization = %q, want configured bearer token", got)
		}
		var request struct {
			Model        string `json:"model"`
			Instructions string `json:"instructions"`
			Input        []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"input"`
			MaxOutputTokens int64 `json:"max_output_tokens"`
			Store           bool  `json:"store"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Model != "gpt-5.1-mini" || request.Instructions != "Характер персонажа\nСтиль ответа" {
			t.Errorf("model/instructions = %q/%q", request.Model, request.Instructions)
		}
		if len(request.Input) != 2 {
			t.Errorf("input items = %d, want 2", len(request.Input))
		} else {
			for index, expected := range []struct{ role, text string }{
				{role: "user", text: "Первое сообщение"},
				{role: "assistant", text: "Первый ответ"},
			} {
				var content string
				if err := json.Unmarshal(request.Input[index].Content, &content); err != nil {
					t.Errorf("input[%d].content is not text: %v", index, err)
				}
				if request.Input[index].Role != expected.role || content != expected.text {
					t.Errorf("input[%d] = %#v with content %q, want %s text %q", index, request.Input[index], content, expected.role, expected.text)
				}
			}
		}
		if request.MaxOutputTokens != MaxOutputTokens {
			t.Errorf("max_output_tokens = %d, want %d", request.MaxOutputTokens, MaxOutputTokens)
		}
		if request.Store {
			t.Error("store = true, want false")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","created_at":1,"model":"gpt-5.1-mini","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"  Продолжение беседы.  "}]}]}`))
	}))
	defer server.Close()

	client := newClient("synthetic-openai-key", "gpt-5.1-mini", server.URL, server.Client())
	got, err := client.Respond(context.Background(), "Характер персонажа\nСтиль ответа", []domain.Message{
		{Role: domain.RoleUser, Text: "Первое сообщение"},
		{Role: domain.RoleAssistant, Text: "Первый ответ"},
	})
	if err != nil {
		t.Fatalf("Respond() error = %v", err)
	}
	if got != "Продолжение беседы." {
		t.Fatalf("Respond() = %q, want trimmed output", got)
	}
}

func TestRespondRejectsInvalidAndOversizedInputBeforeRequest(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client := newClient("synthetic-openai-key", "model", server.URL, server.Client())

	tests := []struct {
		name         string
		instructions string
		history      []domain.Message
	}{
		{name: "empty instructions", instructions: "", history: []domain.Message{{Role: domain.RoleUser, Text: "hello"}}},
		{name: "empty history", instructions: "prompt"},
		{name: "invalid role", instructions: "prompt", history: []domain.Message{{Role: domain.Role("system"), Text: "not allowed"}}},
		{name: "oversized text", instructions: "prompt", history: []domain.Message{{Role: domain.RoleUser, Text: strings.Repeat("x", MaxRequestContentBytes)}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := client.Respond(context.Background(), test.instructions, test.history)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("Respond() error = %v, want ErrInvalidRequest", err)
			}
		})
	}
	if requests != 0 {
		t.Fatalf("HTTP requests = %d, want 0", requests)
	}
}

func TestRespondRejectsEmptyAndIncompleteOutput(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		response  string
		wantError error
	}{
		{name: "empty", response: `{"id":"resp_1","created_at":1,"model":"model","status":"completed","output":[]}`, wantError: ErrEmptyResponse},
		{name: "incomplete", response: `{"id":"resp_1","created_at":1,"model":"model","status":"incomplete","output":[]}`, wantError: ErrIncompleteResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.response))
			}))
			defer server.Close()
			client := newClient("synthetic-openai-key", "model", server.URL, server.Client())
			_, err := client.Respond(context.Background(), "prompt", []domain.Message{{Role: domain.RoleUser, Text: "hello"}})
			if !errors.Is(err, test.wantError) {
				t.Fatalf("Respond() error = %v, want %v", err, test.wantError)
			}
		})
	}
}

func TestRespondPropagatesCancellationWithoutReturningText(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	client := newClient("synthetic-openai-key", "model", server.URL, server.Client())
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		text, err := client.Respond(ctx, "prompt", []domain.Message{{Role: domain.RoleUser, Text: "hello"}})
		if text != "" && err != nil {
			result <- errors.New("Respond() returned text together with an error")
			return
		}
		result <- err
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(time.Second):
		cancel()
		t.Fatal("request did not reach local mock server")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Respond() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Respond() did not return after cancellation")
	}
}

func TestRespondDoesNotExposeAPIErrorBodyOrCredential(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"private diagnostic"}}`))
	}))
	defer server.Close()
	client := newClient("synthetic-openai-key", "model", server.URL, server.Client())
	_, err := client.Respond(context.Background(), "prompt", []domain.Message{{Role: domain.RoleUser, Text: "hello"}})
	if !errors.Is(err, ErrRequestFailed) {
		t.Fatalf("Respond() error = %v, want ErrRequestFailed", err)
	}
	if strings.Contains(err.Error(), "private diagnostic") || strings.Contains(err.Error(), "synthetic-openai-key") {
		t.Fatalf("error leaked response data or credential: %v", err)
	}
}
