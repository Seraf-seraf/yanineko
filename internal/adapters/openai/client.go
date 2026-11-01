package openaiadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/Seraf-seraf/yanineko/internal/domain"
)

const RequestTimeout = 30 * time.Second
const MaxOutputTokens int64 = 1024
const MaxContextTokens int64 = 240_000
const HighPriceThresholdTokens int64 = 272_000
const MaxMemorySearchCalls = 3
const MaxMemoryQueryLength = 1000
const MaxMemoryQueryBytes = 4096
const MaxMemoryArgumentsBytes = 8 * 1024
const MaxMemoryResultBytes = 12 * 1024

var ErrInvalidRequest = errors.New("invalid OpenAI request")
var ErrRequestFailed = errors.New("OpenAI request failed")
var ErrIncompleteResponse = errors.New("OpenAI response was incomplete")
var ErrEmptyResponse = errors.New("OpenAI response contained no text")
var ErrContextTooLarge = errors.New("conversation context exceeds token budget")
var ErrMemoryUnavailable = errors.New("world memory is unavailable")
var ErrMemoryCallLimit = errors.New("world memory call limit exceeded")

const memoryToolName = "search_world_memory"

type Client struct {
	sdk             openai.Client
	model           string
	reasoningEffort shared.ReasoningEffort
}

func New(apiKey, model, reasoningEffort, baseURL string) *Client {
	client := newClient(apiKey, model, baseURL, nil)
	client.reasoningEffort = shared.ReasoningEffort(reasoningEffort)
	return client
}

func newClient(apiKey, model, baseURL string, httpClient *http.Client) *Client {
	options := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithMaxRetries(0),
		option.WithRequestTimeout(RequestTimeout),
	}
	if baseURL != "" {
		options = append(options, option.WithBaseURL(baseURL))
	}
	if httpClient != nil {
		options = append(options, option.WithHTTPClient(httpClient))
	}
	return &Client{sdk: openai.NewClient(options...), model: model, reasoningEffort: shared.ReasoningEffortLow}
}

func (c *Client) Respond(ctx context.Context, request domain.AgentRequest, memory domain.AgentMemory) (domain.AgentResult, error) {
	if c == nil || c.model == "" || strings.TrimSpace(request.Instructions) == "" || len(request.Messages) == 0 {
		return domain.AgentResult{}, ErrInvalidRequest
	}
	items, err := decodeContext(request.Context)
	if err != nil {
		return domain.AgentResult{}, err
	}
	for _, message := range request.Messages {
		if message.Role != domain.RoleUser && message.Role != domain.RoleAssistant || strings.TrimSpace(message.Text) == "" {
			return domain.AgentResult{}, ErrInvalidRequest
		}
		items = append(items, responses.ResponseInputItemParamOfMessage(message.Text, responses.EasyInputMessageRole(message.Role)))
	}

	memoryCalls := 0
	for {
		tools := memoryTools(memory != nil && memoryCalls < MaxMemorySearchCalls)
		input, err := c.prepareInput(ctx, request.Instructions, items, tools)
		if err != nil {
			return domain.AgentResult{}, err
		}
		items = input
		response, err := c.createResponse(ctx, request.Instructions, items, tools)
		if err != nil {
			return domain.AgentResult{}, err
		}
		if response == nil || response.Status != responses.ResponseStatusCompleted {
			return domain.AgentResult{}, ErrIncompleteResponse
		}
		if err := appendResponseOutput(&items, response.Output); err != nil {
			return domain.AgentResult{}, err
		}
		calls, err := functionCalls(response.Output)
		if err != nil {
			return domain.AgentResult{}, err
		}
		if len(calls) == 0 {
			text := limitOutputText(strings.TrimSpace(response.OutputText()))
			if text == "" {
				return domain.AgentResult{}, ErrEmptyResponse
			}
			if err := replaceLastAssistantMessage(&items, text); err != nil {
				return domain.AgentResult{}, err
			}
			itemsJSON, err := marshalInputItems(items)
			if err != nil {
				return domain.AgentResult{}, err
			}
			return domain.AgentResult{Text: text, ItemsJSON: itemsJSON}, nil
		}
		if memory == nil {
			return domain.AgentResult{}, ErrMemoryUnavailable
		}
		if memoryCalls+len(calls) > MaxMemorySearchCalls {
			return domain.AgentResult{}, ErrMemoryCallLimit
		}
		for _, call := range calls {
			output, err := runMemoryCall(ctx, memory, call)
			if err != nil {
				return domain.AgentResult{}, err
			}
			functionOutput := responses.ResponseInputItemParamOfFunctionCallOutput(output)
			functionOutput.OfFunctionCallOutput.CallID = param.NewOpt(call.CallID)
			items = append(items, functionOutput)
			memoryCalls++
		}
	}
}

func (c *Client) prepareInput(ctx context.Context, instructions string, items []responses.ResponseInputItemUnionParam, tools []responses.ToolUnionParam) ([]responses.ResponseInputItemUnionParam, error) {
	count, err := c.countInputTokens(ctx, instructions, items, tools)
	if err != nil {
		return nil, err
	}
	if count+MaxOutputTokens <= MaxContextTokens {
		return items, nil
	}
	compacted, err := c.compact(ctx, instructions, items)
	if err != nil {
		return nil, err
	}
	count, err = c.countInputTokens(ctx, instructions, compacted, tools)
	if err != nil {
		return nil, err
	}
	if count+MaxOutputTokens > MaxContextTokens {
		return nil, ErrContextTooLarge
	}
	return compacted, nil
}

func (c *Client) countInputTokens(ctx context.Context, instructions string, items []responses.ResponseInputItemUnionParam, tools []responses.ToolUnionParam) (int64, error) {
	requestContext, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	params := responses.InputTokenCountParams{
		Instructions: param.NewOpt(instructions),
		Input:        responses.InputTokenCountParamsInputUnion{OfResponseInputItemArray: items},
		Model:        param.NewOpt(c.model),
		Reasoning:    shared.ReasoningParam{Effort: c.reasoningEffort},
		Truncation:   responses.InputTokenCountParamsTruncationDisabled,
		Tools:        tools,
	}
	if len(tools) > 0 {
		params.ToolChoice = responses.InputTokenCountParamsToolChoiceUnion{OfToolChoiceMode: param.NewOpt(responses.ToolChoiceOptionsAuto)}
		params.ParallelToolCalls = param.NewOpt(false)
	}
	count, err := c.sdk.Responses.InputTokens.Count(requestContext, params)
	if err != nil {
		return 0, apiError(err)
	}
	if count == nil || count.InputTokens < 0 {
		return 0, ErrRequestFailed
	}
	return count.InputTokens, nil
}

func (c *Client) compact(ctx context.Context, instructions string, items []responses.ResponseInputItemUnionParam) ([]responses.ResponseInputItemUnionParam, error) {
	count, err := c.countInputTokens(ctx, instructions, items, nil)
	if err != nil {
		return nil, err
	}
	if count >= HighPriceThresholdTokens {
		return nil, ErrContextTooLarge
	}
	requestContext, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	compacted, err := c.sdk.Responses.Compact(requestContext, responses.ResponseCompactParams{
		Model:        responses.ResponseCompactParamsModel(c.model),
		Instructions: param.NewOpt(instructions),
		Input:        responses.ResponseCompactParamsInputUnion{OfResponseInputItemArray: items},
	})
	if err != nil {
		return nil, apiError(err)
	}
	if compacted == nil || len(compacted.Output) == 0 {
		return nil, ErrIncompleteResponse
	}
	result := make([]responses.ResponseInputItemUnionParam, 0, len(compacted.Output))
	if err := appendResponseOutput(&result, compacted.Output); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) createResponse(ctx context.Context, instructions string, items []responses.ResponseInputItemUnionParam, tools []responses.ToolUnionParam) (*responses.Response, error) {
	params := responses.ResponseNewParams{
		Instructions:    param.NewOpt(instructions),
		Input:           responses.ResponseNewParamsInputUnion{OfInputItemList: items},
		MaxOutputTokens: param.NewOpt(MaxOutputTokens),
		Model:           shared.ResponsesModel(c.model),
		Reasoning:       shared.ReasoningParam{Effort: c.reasoningEffort},
		Store:           param.NewOpt(false),
		Truncation:      responses.ResponseNewParamsTruncationDisabled,
		Include:         []responses.ResponseIncludable{responses.ResponseIncludableReasoningEncryptedContent},
		Tools:           tools,
	}
	if len(tools) > 0 {
		params.ToolChoice = responses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: param.NewOpt(responses.ToolChoiceOptionsAuto)}
		params.ParallelToolCalls = param.NewOpt(false)
	}
	requestContext, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	response, err := c.sdk.Responses.New(requestContext, params)
	if err != nil {
		return nil, apiError(err)
	}
	return response, nil
}

func memoryTools(enabled bool) []responses.ToolUnionParam {
	if !enabled {
		return nil
	}
	return []responses.ToolUnionParam{{OfFunction: &responses.FunctionToolParam{
		Name:        memoryToolName,
		Description: param.NewOpt("Ищет подтверждённые локальные сведения о мире, персонажах и событиях Яни Нэко. Вызывай только когда ответ зависит от факта канона."),
		Strict:      param.NewOpt(true),
		Parameters: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"query": map[string]any{"type": "string", "minLength": 1, "maxLength": MaxMemoryQueryLength}},
			"required":             []string{"query"},
			"additionalProperties": false,
		},
	}}}
}

type memoryCall struct {
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func functionCalls(items []responses.ResponseOutputItemUnion) ([]memoryCall, error) {
	calls := make([]memoryCall, 0, len(items))
	for _, item := range items {
		if item.Type != "function_call" {
			continue
		}
		var call memoryCall
		if err := json.Unmarshal([]byte(item.RawJSON()), &call); err != nil || call.Name == "" || call.CallID == "" || call.Arguments == "" {
			return nil, ErrInvalidRequest
		}
		if call.Name != memoryToolName {
			return nil, ErrInvalidRequest
		}
		calls = append(calls, call)
	}
	return calls, nil
}

func runMemoryCall(ctx context.Context, memory domain.AgentMemory, call memoryCall) (string, error) {
	if len(call.Arguments) > MaxMemoryArgumentsBytes {
		return "", ErrInvalidRequest
	}
	var arguments struct {
		Query string `json:"query"`
	}
	decoder := json.NewDecoder(strings.NewReader(call.Arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&arguments); err != nil {
		return "", ErrInvalidRequest
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return "", ErrInvalidRequest
	}
	query := strings.TrimSpace(arguments.Query)
	if query == "" || len(query) > MaxMemoryQueryBytes || utf8.RuneCountInString(query) > MaxMemoryQueryLength {
		return "", ErrInvalidRequest
	}
	output, err := memory.Search(ctx, query)
	if err != nil {
		return "", errors.Join(ErrMemoryUnavailable, err)
	}
	if len(output) > MaxMemoryResultBytes || !json.Valid([]byte(output)) {
		return "", ErrInvalidRequest
	}
	return output, nil
}

func appendResponseOutput(items *[]responses.ResponseInputItemUnionParam, output []responses.ResponseOutputItemUnion) error {
	for _, item := range output {
		if !json.Valid([]byte(item.RawJSON())) {
			return ErrInvalidRequest
		}
		var inputItem responses.ResponseInputItemUnion
		if err := json.Unmarshal([]byte(item.RawJSON()), &inputItem); err != nil {
			return ErrInvalidRequest
		}
		*items = append(*items, inputItem.ToParam())
	}
	return nil
}

func decodeContext(modelContext domain.ModelContext) ([]responses.ResponseInputItemUnionParam, error) {
	if !modelContext.Initialized {
		return nil, nil
	}
	if len(modelContext.ItemsJSON) == 0 || !json.Valid(modelContext.ItemsJSON) || modelContext.ItemsJSON[0] != '[' {
		return nil, ErrInvalidRequest
	}
	var items []responses.ResponseInputItemUnion
	if err := json.Unmarshal(modelContext.ItemsJSON, &items); err != nil {
		return nil, ErrInvalidRequest
	}
	result := make([]responses.ResponseInputItemUnionParam, 0, len(items))
	for _, item := range items {
		if !json.Valid([]byte(item.RawJSON())) {
			return nil, ErrInvalidRequest
		}
		result = append(result, item.ToParam())
	}
	return result, nil
}

func marshalInputItems(items []responses.ResponseInputItemUnionParam) (json.RawMessage, error) {
	data, err := json.Marshal(items)
	if err != nil || !json.Valid(data) {
		return nil, ErrInvalidRequest
	}
	return json.RawMessage(data), nil
}

func replaceLastAssistantMessage(items *[]responses.ResponseInputItemUnionParam, text string) error {
	for index := len(*items) - 1; index >= 0; index-- {
		data, err := json.Marshal((*items)[index])
		if err != nil {
			return ErrInvalidRequest
		}
		var message struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if err := json.Unmarshal(data, &message); err != nil {
			return ErrInvalidRequest
		}
		if message.Type == "message" && message.Role == string(domain.RoleAssistant) {
			(*items)[index] = responses.ResponseInputItemParamOfMessage(text, responses.EasyInputMessageRoleAssistant)
			return nil
		}
	}
	return ErrInvalidRequest
}

func limitOutputText(text string) string {
	runes := []rune(text)
	if len(runes) <= domain.MaxOutgoingTextRunes {
		return text
	}
	return string(runes[:domain.MaxOutgoingTextRunes])
}

func apiError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var apiError *openai.Error
	if errors.As(err, &apiError) {
		return errors.Join(ErrRequestFailed, statusCodeError{statusCode: apiError.StatusCode})
	}
	return ErrRequestFailed
}

type statusCodeError struct {
	statusCode int
}

func (e statusCodeError) Error() string {
	return "HTTP status " + http.StatusText(e.statusCode)
}
