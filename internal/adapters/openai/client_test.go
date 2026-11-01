package openaiadapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Seraf-seraf/yanineko/internal/domain"
	"github.com/openai/openai-go/v3/shared"
)

func TestRespondUsesResponsesAPIAndReturnsOpaqueState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/responses/input_tokens"):
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode token-count request: %v", err)
			}
			if request["model"] != "gpt-6-luna" || request["truncation"] != "disabled" {
				t.Errorf("token-count model/truncation = %v/%v", request["model"], request["truncation"])
			}
			writeTokenCount(w, 120)
		case strings.HasSuffix(r.URL.Path, "/responses"):
			var request struct {
				Model        string `json:"model"`
				Instructions string `json:"instructions"`
				Input        []struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				} `json:"input"`
				MaxOutputTokens int64  `json:"max_output_tokens"`
				Store           bool   `json:"store"`
				Truncation      string `json:"truncation"`
				Reasoning       struct {
					Effort string `json:"effort"`
				} `json:"reasoning"`
			}
			if got := r.Header.Get("Authorization"); got != "Bearer synthetic-openai-key" {
				t.Errorf("Authorization = %q, want configured bearer token", got)
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode response request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if request.Model != "gpt-6-luna" || request.Instructions != "Характер персонажа\nСтиль ответа" {
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
			if request.MaxOutputTokens != MaxOutputTokens || request.Store || request.Truncation != "disabled" || request.Reasoning.Effort != "low" {
				t.Errorf("response controls = output:%d store:%t truncation:%q reasoning:%q", request.MaxOutputTokens, request.Store, request.Truncation, request.Reasoning.Effort)
			}
			writeResponse(w, `{"id":"resp_1","created_at":1,"model":"gpt-6-luna","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"  Продолжение беседы.  "}]}]}`)
		default:
			t.Errorf("unexpected request path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newClient("synthetic-openai-key", "gpt-6-luna", server.URL, server.Client())
	got, err := client.Respond(context.Background(), domain.AgentRequest{
		Instructions: "Характер персонажа\nСтиль ответа",
		Messages: []domain.Message{
			{Role: domain.RoleUser, Text: "Первое сообщение"},
			{Role: domain.RoleAssistant, Text: "Первый ответ"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("Respond() error = %v", err)
	}
	if got.Text != "Продолжение беседы." {
		t.Fatalf("Respond().Text = %q, want trimmed output", got.Text)
	}
	if !json.Valid(got.ItemsJSON) || got.ItemsJSON[0] != '[' {
		t.Fatalf("Respond().ItemsJSON = %s, want JSON array", got.ItemsJSON)
	}
}

func TestRespondUsesConfiguredReasoningEffort(t *testing.T) {
	server := newBasicResponseServer(t, `{"id":"resp_1","created_at":1,"model":"model","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ответ"}]}]}`, func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/responses") {
			var request struct {
				Reasoning struct {
					Effort string `json:"effort"`
				} `json:"reasoning"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode request: %v", err)
			}
			if request.Reasoning.Effort != "medium" {
				t.Errorf("reasoning.effort = %q, want medium", request.Reasoning.Effort)
			}
		}
	})
	defer server.Close()

	client := newClient("synthetic-openai-key", "model", server.URL, server.Client())
	client.reasoningEffort = shared.ReasoningEffortMedium
	if _, err := client.Respond(context.Background(), requestWithUserMessage(), nil); err != nil {
		t.Fatalf("Respond() error = %v", err)
	}
}

func TestRespondRejectsInvalidInputBeforeRequest(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client := newClient("synthetic-openai-key", "model", server.URL, server.Client())

	tests := []struct {
		name    string
		request domain.AgentRequest
	}{
		{name: "empty instructions", request: domain.AgentRequest{Messages: []domain.Message{{Role: domain.RoleUser, Text: "hello"}}}},
		{name: "empty history", request: domain.AgentRequest{Instructions: "prompt"}},
		{name: "invalid role", request: domain.AgentRequest{Instructions: "prompt", Messages: []domain.Message{{Role: domain.Role("system"), Text: "not allowed"}}}},
		{name: "empty message", request: domain.AgentRequest{Instructions: "prompt", Messages: []domain.Message{{Role: domain.RoleUser, Text: "  "}}}},
		{name: "invalid model context", request: domain.AgentRequest{Instructions: "prompt", Context: domain.ModelContext{Initialized: true, ItemsJSON: []byte("not json")}, Messages: []domain.Message{{Role: domain.RoleUser, Text: "hello"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := client.Respond(context.Background(), test.request, nil)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("Respond() error = %v, want ErrInvalidRequest", err)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("HTTP requests = %d, want 0", requests.Load())
	}
}

func TestRespondRejectsEmptyAndIncompleteOutput(t *testing.T) {
	for _, test := range []struct {
		name      string
		response  string
		wantError error
	}{
		{name: "empty", response: `{"id":"resp_1","created_at":1,"model":"model","status":"completed","output":[]}`, wantError: ErrEmptyResponse},
		{name: "incomplete", response: `{"id":"resp_1","created_at":1,"model":"model","status":"incomplete","output":[]}`, wantError: ErrIncompleteResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newBasicResponseServer(t, test.response, nil)
			defer server.Close()
			client := newClient("synthetic-openai-key", "model", server.URL, server.Client())
			_, err := client.Respond(context.Background(), requestWithUserMessage(), nil)
			if !errors.Is(err, test.wantError) {
				t.Fatalf("Respond() error = %v, want %v", err, test.wantError)
			}
		})
	}
}

func TestRespondStoresTheSameTextThatFitsTelegram(t *testing.T) {
	text := strings.Repeat("я", domain.MaxOutgoingTextRunes+17)
	response := `{"id":"resp_1","created_at":1,"model":"model","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}]}`
	server := newBasicResponseServer(t, response, nil)
	defer server.Close()
	client := newClient("synthetic-openai-key", "model", server.URL, server.Client())

	result, err := client.Respond(context.Background(), requestWithUserMessage(), nil)
	if err != nil {
		t.Fatalf("Respond() error = %v", err)
	}
	if len([]rune(result.Text)) != domain.MaxOutgoingTextRunes || result.Text != strings.Repeat("я", domain.MaxOutgoingTextRunes) {
		t.Fatalf("returned text has %d runes, want exactly %d", len([]rune(result.Text)), domain.MaxOutgoingTextRunes)
	}
	if strings.Contains(string(result.ItemsJSON), text) || !strings.Contains(string(result.ItemsJSON), result.Text) {
		t.Fatalf("opaque state does not match the text delivered to Telegram")
	}
}

func TestRespondRunsLocalWorldMemoryTool(t *testing.T) {
	var responseCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/responses/input_tokens"):
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode token-count request: %v", err)
			}
			if request["tool_choice"] != "auto" || request["parallel_tool_calls"] != false {
				t.Errorf("tool count controls = %v/%v", request["tool_choice"], request["parallel_tool_calls"])
			}
			writeTokenCount(w, 100)
		case strings.HasSuffix(r.URL.Path, "/responses"):
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode response request: %v", err)
			}
			if request["tool_choice"] != "auto" || request["parallel_tool_calls"] != false || request["store"] != false {
				t.Errorf("response controls = tool:%v parallel:%v store:%v", request["tool_choice"], request["parallel_tool_calls"], request["store"])
			}
			tools, ok := request["tools"].([]any)
			if !ok || len(tools) != 1 {
				t.Errorf("tools = %v, want one local memory tool", request["tools"])
			} else {
				tool, _ := tools[0].(map[string]any)
				if tool["name"] != memoryToolName || tool["strict"] != true {
					t.Errorf("tool config = %v", tool)
				}
			}
			if responseCalls.Add(1) == 1 {
				writeResponse(w, `{"id":"resp_tool","created_at":1,"model":"model","status":"completed","output":[{"id":"fc_1","type":"function_call","call_id":"call_1","name":"search_world_memory","arguments":"{\"query\":\"кто такая Ая\"}","status":"completed"}]}`)
				return
			}
			writeResponse(w, `{"id":"resp_final","created_at":2,"model":"model","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Ая — персонаж сериала."}]}]}`)
		default:
			t.Errorf("unexpected request path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	memory := &fakeMemory{result: `{"facts":[{"subject":"Ая","canon":"anime"}]}`}
	client := newClient("synthetic-openai-key", "model", server.URL, server.Client())
	result, err := client.Respond(context.Background(), requestWithUserMessage(), memory)
	if err != nil {
		t.Fatalf("Respond() error = %v", err)
	}
	if result.Text != "Ая — персонаж сериала." || memory.query != "кто такая Ая" {
		t.Fatalf("result/query = %q/%q", result.Text, memory.query)
	}
	if responseCalls.Load() != 2 {
		t.Fatalf("response calls = %d, want 2", responseCalls.Load())
	}
	if !strings.Contains(string(result.ItemsJSON), "function_call_output") || !strings.Contains(string(result.ItemsJSON), "search_world_memory") {
		t.Fatalf("opaque state lost tool exchange: %s", result.ItemsJSON)
	}
}

func TestRunMemoryCallValidatesJSONAndAllowsRussianQueriesByRuneCount(t *testing.T) {
	memory := &fakeMemory{result: `{"facts":[]}`}
	for _, arguments := range []string{
		`{"query":"Янико","extra":true}`,
		`{"query":"Янико"} {}`,
		`{"query":"` + strings.Repeat("я", MaxMemoryQueryLength+1) + `"}`,
		strings.Repeat("x", MaxMemoryArgumentsBytes+1),
	} {
		if _, err := runMemoryCall(context.Background(), memory, memoryCall{CallID: "call", Name: memoryToolName, Arguments: arguments}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("runMemoryCall(invalid) error = %v, want ErrInvalidRequest", err)
		}
	}
	if memory.query != "" {
		t.Fatalf("memory search was called for invalid arguments with query %q", memory.query)
	}

	query := strings.Repeat("я", 600)
	if _, err := runMemoryCall(context.Background(), memory, memoryCall{CallID: "call", Name: memoryToolName, Arguments: `{"query":"` + query + `"}`}); err != nil {
		t.Fatalf("runMemoryCall(valid Russian query) error = %v", err)
	}
	if memory.query != query {
		t.Fatalf("memory query length = %d bytes, want all %d bytes", len(memory.query), len(query))
	}
}

func TestRespondCompactsBeforeCrossingContextPriceThreshold(t *testing.T) {
	var countCalls atomic.Int64
	var compactCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/responses/input_tokens"):
			call := countCalls.Add(1)
			if call == 1 {
				writeTokenCount(w, MaxContextTokens)
			} else {
				writeTokenCount(w, 100)
			}
		case strings.HasSuffix(r.URL.Path, "/responses/compact"):
			compactCalls.Add(1)
			writeResponse(w, `{"id":"compact_1","created_at":1,"object":"response.compaction","output":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},{"type":"compaction","encrypted_content":"opaque compacted state"}],"usage":{"input_tokens":200000,"output_tokens":100,"total_tokens":200100}}`)
		case strings.HasSuffix(r.URL.Path, "/responses"):
			var request struct {
				Input []map[string]any `json:"input"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode response request: %v", err)
			}
			compactionPreserved := false
			for _, item := range request.Input {
				if item["type"] == "compaction" && item["encrypted_content"] == "opaque compacted state" {
					compactionPreserved = true
				}
			}
			if !compactionPreserved {
				t.Errorf("next response input lost canonical compaction output: %#v", request.Input)
			}
			if len(request.Input) != 2 || request.Input[0]["type"] != "message" || request.Input[0]["role"] != "user" {
				t.Errorf("next response input pruned retained message from compaction: %#v", request.Input)
			}
			writeResponse(w, `{"id":"resp_1","created_at":2,"model":"model","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ответ"}]}]}`)
		default:
			t.Errorf("unexpected request path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newClient("synthetic-openai-key", "model", server.URL, server.Client())
	if _, err := client.Respond(context.Background(), requestWithUserMessage(), nil); err != nil {
		t.Fatalf("Respond() error = %v", err)
	}
	if countCalls.Load() != 3 || compactCalls.Load() != 1 {
		t.Fatalf("token-count/compact calls = %d/%d, want 3/1", countCalls.Load(), compactCalls.Load())
	}
}

func TestRespondRefusesCompactionAtHighPriceThreshold(t *testing.T) {
	var countCalls atomic.Int64
	var compactCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/responses/input_tokens") {
			if countCalls.Add(1) == 1 {
				writeTokenCount(w, MaxContextTokens)
			} else {
				writeTokenCount(w, HighPriceThresholdTokens)
			}
			return
		}
		if strings.HasSuffix(r.URL.Path, "/responses/compact") {
			compactCalls.Add(1)
		}
		t.Errorf("unexpected request %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := newClient("synthetic-openai-key", "model", server.URL, server.Client())
	_, err := client.Respond(context.Background(), requestWithUserMessage(), nil)
	if !errors.Is(err, ErrContextTooLarge) {
		t.Fatalf("Respond() error = %v, want ErrContextTooLarge", err)
	}
	if countCalls.Load() != 2 || compactCalls.Load() != 0 {
		t.Fatalf("token-count/compact calls = %d/%d, want 2/0", countCalls.Load(), compactCalls.Load())
	}
}

func TestRespondPropagatesCancellationWithoutReturningText(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses/input_tokens") {
			t.Errorf("request path = %s, want token count", r.URL.Path)
		}
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
			writeTokenCount(w, 100)
		}
	}))
	defer server.Close()
	defer close(release)
	client := newClient("synthetic-openai-key", "model", server.URL, server.Client())
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		response, err := client.Respond(ctx, requestWithUserMessage(), nil)
		if response.Text != "" && err != nil {
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"private diagnostic"}}`))
	}))
	defer server.Close()
	client := newClient("synthetic-openai-key", "model", server.URL, server.Client())
	_, err := client.Respond(context.Background(), requestWithUserMessage(), nil)
	if !errors.Is(err, ErrRequestFailed) {
		t.Fatalf("Respond() error = %v, want ErrRequestFailed", err)
	}
	if strings.Contains(err.Error(), "private diagnostic") || strings.Contains(err.Error(), "synthetic-openai-key") {
		t.Fatalf("error leaked response data or credential: %v", err)
	}
}

type fakeMemory struct {
	query  string
	result string
}

func (m *fakeMemory) Search(_ context.Context, query string) (string, error) {
	m.query = query
	return m.result, nil
}

func requestWithUserMessage() domain.AgentRequest {
	return domain.AgentRequest{Instructions: "prompt", Messages: []domain.Message{{Role: domain.RoleUser, Text: "hello"}}}
}

func newBasicResponseServer(t *testing.T, response string, inspect func(*http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if inspect != nil {
			inspect(r)
		}
		if strings.HasSuffix(r.URL.Path, "/responses/input_tokens") {
			writeTokenCount(w, 100)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/responses") {
			writeResponse(w, response)
			return
		}
		t.Errorf("unexpected request path %s", r.URL.Path)
		http.NotFound(w, r)
	}))
}

func writeTokenCount(w http.ResponseWriter, tokens int64) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"object":"response.input_tokens","input_tokens":` + strconv.FormatInt(tokens, 10) + `}`))
}

func writeResponse(w http.ResponseWriter, response string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(response))
}
