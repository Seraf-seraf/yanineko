package openaiadapter

import (
	"context"
	"encoding/base64"
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
	"github.com/Seraf-seraf/yanineko/internal/observability"
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
const MaxMediaImageBytes = 4 << 20
const MaxMediaRequestBytes = 10 << 20
const MaxMediaDescriptionRunes = 800
const MediaDescriptionTimeout = 10 * time.Second

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
		observability.Start(ctx, request.UpdateID, "agent_request_validation").Fail(ctx, "invalid_request")
		return domain.AgentResult{}, ErrInvalidRequest
	}
	items, err := decodeContext(request.Context)
	if err != nil {
		observability.Start(ctx, request.UpdateID, "model_context_validation").Fail(ctx, "invalid_model_context")
		return domain.AgentResult{}, err
	}
	for _, message := range request.Messages {
		if message.Role != domain.RoleUser && message.Role != domain.RoleAssistant || strings.TrimSpace(message.Text) == "" {
			observability.Start(ctx, request.UpdateID, "agent_request_validation").Fail(ctx, "invalid_message")
			return domain.AgentResult{}, ErrInvalidRequest
		}
		items = append(items, responses.ResponseInputItemParamOfMessage(message.Text, responses.EasyInputMessageRole(message.Role)))
	}

	turnStage := observability.Start(ctx, request.UpdateID, "agent_turn")
	memoryCalls := 0
	responseCalls := 0
	for {
		responseCalls++
		tools := memoryTools(memory != nil && memoryCalls < MaxMemorySearchCalls)
		input, err := c.prepareInput(ctx, request.UpdateID, request.Instructions, items, tools, responseCalls)
		if err != nil {
			return domain.AgentResult{}, err
		}
		items = input
		response, err := c.createResponse(ctx, request.UpdateID, responseCalls, request.Instructions, items, tools)
		if err != nil {
			return domain.AgentResult{}, err
		}
		if response == nil || response.Status != responses.ResponseStatusCompleted {
			observability.Start(ctx, request.UpdateID, "response_validation").Fail(ctx, "incomplete_response", "response_status", responseStatus(response))
			return domain.AgentResult{}, ErrIncompleteResponse
		}
		if err := appendResponseOutput(&items, response.Output); err != nil {
			observability.Start(ctx, request.UpdateID, "response_output_validation").Fail(ctx, "invalid_response_output")
			return domain.AgentResult{}, err
		}
		calls, err := functionCalls(response.Output)
		if err != nil {
			observability.Start(ctx, request.UpdateID, "response_tool_validation").Fail(ctx, "invalid_tool_call")
			return domain.AgentResult{}, err
		}
		if len(calls) == 0 {
			text := limitOutputText(strings.TrimSpace(response.OutputText()))
			if text == "" {
				observability.Start(ctx, request.UpdateID, "response_validation").Fail(ctx, "empty_response")
				return domain.AgentResult{}, ErrEmptyResponse
			}
			if err := replaceLastAssistantMessage(&items, text); err != nil {
				observability.Start(ctx, request.UpdateID, "response_context_update").Fail(ctx, "assistant_message_not_found")
				return domain.AgentResult{}, err
			}
			itemsJSON, err := marshalInputItems(items)
			if err != nil {
				observability.Start(ctx, request.UpdateID, "response_context_serialization").Fail(ctx, "model_context_serialization_failed")
				return domain.AgentResult{}, err
			}
			turnStage.Complete(ctx, "response_call_count", responseCalls, "memory_call_count", memoryCalls)
			return domain.AgentResult{Text: text, ItemsJSON: itemsJSON}, nil
		}
		if memory == nil {
			observability.Start(ctx, request.UpdateID, "response_tool_dispatch").Fail(ctx, "memory_unavailable", "tool_call_count", len(calls))
			return domain.AgentResult{}, ErrMemoryUnavailable
		}
		if memoryCalls+len(calls) > MaxMemorySearchCalls {
			observability.Start(ctx, request.UpdateID, "response_tool_dispatch").Fail(ctx, "memory_call_limit_exceeded", "tool_call_count", len(calls), "call_count", memoryCalls)
			return domain.AgentResult{}, ErrMemoryCallLimit
		}
		for _, call := range calls {
			output, err := runMemoryCall(ctx, request.UpdateID, memoryCalls+1, memory, call)
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

// Describe produces a text-only description so image bytes never enter the
// archived conversation or the persisted Responses API context.
func (c *Client) Describe(ctx context.Context, updateID int64, images []domain.Image) (string, error) {
	if c == nil || c.model == "" || len(images) == 0 || len(images) > 4 {
		return "", ErrInvalidRequest
	}
	content := responses.ResponseInputMessageContentListParam{
		responses.ResponseInputContentParamOfInputText("Опиши кратко изображения и последовательность кадров: видимые объекты, текст и заметные действия. Не додумывай невидимые детали."),
	}
	encodedBytes := int64(0)
	for _, image := range images {
		if image.MIMEType != "image/png" || len(image.Data) == 0 || len(image.Data) > MaxMediaImageBytes {
			return "", ErrInvalidRequest
		}
		encodedBytes += int64(len(image.Data))
		if encodedBytes > MaxMediaRequestBytes {
			return "", ErrInvalidRequest
		}
		imageContent := responses.ResponseInputContentParamOfInputImage(responses.ResponseInputImageDetailAuto)
		imageContent.OfInputImage.ImageURL = param.NewOpt("data:" + image.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(image.Data))
		content = append(content, imageContent)
	}
	stage := observability.Start(ctx, updateID, "openai_media_description")
	requestContext, cancel := context.WithTimeout(ctx, MediaDescriptionTimeout)
	defer cancel()
	response, err := c.sdk.Responses.New(requestContext, responses.ResponseNewParams{
		Instructions: param.NewOpt("Ты описываешь пользовательские изображения для сохранения в текстовой истории диалога."),
		Input: responses.ResponseNewParamsInputUnion{OfInputItemList: responses.ResponseInputParam{
			responses.ResponseInputItemParamOfMessage(content, responses.EasyInputMessageRoleUser),
		}},
		MaxOutputTokens: param.NewOpt[int64](512),
		Model:           shared.ResponsesModel(c.model),
		Store:           param.NewOpt(false),
		Truncation:      responses.ResponseNewParamsTruncationDisabled,
	})
	if err != nil {
		requestErr := apiError(err)
		stage.Fail(ctx, openAIErrorCode(requestErr), openAIStatusFields(requestErr)...)
		return "", requestErr
	}
	if response == nil || response.Status != responses.ResponseStatusCompleted {
		stage.Fail(ctx, "incomplete_response", "response_status", responseStatus(response))
		return "", ErrIncompleteResponse
	}
	description := strings.TrimSpace(response.OutputText())
	if description == "" {
		stage.Fail(ctx, "empty_description")
		return "", ErrEmptyResponse
	}
	runes := []rune(description)
	if len(runes) > MaxMediaDescriptionRunes {
		description = string(runes[:MaxMediaDescriptionRunes])
	}
	stage.Complete(ctx, "image_count", len(images), "description_runes", len([]rune(description)))
	return description, nil
}

func (c *Client) prepareInput(ctx context.Context, updateID int64, instructions string, items []responses.ResponseInputItemUnionParam, tools []responses.ToolUnionParam, attempt int) ([]responses.ResponseInputItemUnionParam, error) {
	count, err := c.countInputTokens(ctx, updateID, "preflight", instructions, items, tools, attempt)
	if err != nil {
		return nil, err
	}
	if count+MaxOutputTokens <= MaxContextTokens {
		observability.Start(ctx, updateID, "context_budget").Complete(ctx, "input_tokens", count, "output_reserve_tokens", MaxOutputTokens, "compaction", false)
		return items, nil
	}
	compactionStage := observability.Start(ctx, updateID, "context_compaction")
	compacted, err := c.compact(ctx, updateID, instructions, items, attempt)
	if err != nil {
		return nil, err
	}
	compactedCount, err := c.countInputTokens(ctx, updateID, "post_compaction", instructions, compacted, tools, attempt)
	if err != nil {
		return nil, err
	}
	if compactedCount+MaxOutputTokens > MaxContextTokens {
		compactionStage.Fail(ctx, "context_still_too_large", "input_tokens_before", count, "input_tokens_after", compactedCount, "context_limit_tokens", MaxContextTokens)
		return nil, ErrContextTooLarge
	}
	compactionStage.Complete(ctx, "input_tokens_before", count, "input_tokens_after", compactedCount, "context_limit_tokens", MaxContextTokens)
	return compacted, nil
}

func (c *Client) countInputTokens(ctx context.Context, updateID int64, purpose string, instructions string, items []responses.ResponseInputItemUnionParam, tools []responses.ToolUnionParam, attempt int) (int64, error) {
	stage := observability.Start(ctx, updateID, "openai_input_token_count")
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
		requestErr := apiError(err)
		stage.Fail(ctx, openAIErrorCode(requestErr), append([]any{"purpose", purpose, "attempt", attempt}, openAIStatusFields(requestErr)...)...)
		return 0, requestErr
	}
	if count == nil || count.InputTokens < 0 {
		stage.Fail(ctx, "invalid_token_count", "purpose", purpose, "attempt", attempt)
		return 0, ErrRequestFailed
	}
	stage.Complete(ctx, "purpose", purpose, "attempt", attempt, "model", c.model, "reasoning_effort", string(c.reasoningEffort), "input_tokens", count.InputTokens, "tool_enabled", len(tools) > 0)
	return count.InputTokens, nil
}

func (c *Client) compact(ctx context.Context, updateID int64, instructions string, items []responses.ResponseInputItemUnionParam, attempt int) ([]responses.ResponseInputItemUnionParam, error) {
	count, err := c.countInputTokens(ctx, updateID, "compaction_safety", instructions, items, nil, attempt)
	if err != nil {
		return nil, err
	}
	if count >= HighPriceThresholdTokens {
		observability.Start(ctx, updateID, "responses_compact").Fail(ctx, "high_price_threshold_reached", "input_tokens", count, "threshold_tokens", HighPriceThresholdTokens)
		return nil, ErrContextTooLarge
	}
	stage := observability.Start(ctx, updateID, "responses_compact")
	requestContext, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	compacted, err := c.sdk.Responses.Compact(requestContext, responses.ResponseCompactParams{
		Model:        responses.ResponseCompactParamsModel(c.model),
		Instructions: param.NewOpt(instructions),
		Input:        responses.ResponseCompactParamsInputUnion{OfResponseInputItemArray: items},
	})
	if err != nil {
		requestErr := apiError(err)
		stage.Fail(ctx, openAIErrorCode(requestErr), append([]any{"attempt", attempt}, openAIStatusFields(requestErr)...)...)
		return nil, requestErr
	}
	if compacted == nil || len(compacted.Output) == 0 {
		stage.Fail(ctx, "incomplete_compaction_response", "attempt", attempt)
		return nil, ErrIncompleteResponse
	}
	result := make([]responses.ResponseInputItemUnionParam, 0, len(compacted.Output))
	if err := appendResponseOutput(&result, compacted.Output); err != nil {
		stage.Fail(ctx, "invalid_compaction_output", "attempt", attempt)
		return nil, err
	}
	stage.Complete(ctx, "attempt", attempt, "input_item_count", len(items), "output_item_count", len(result), "model", c.model)
	return result, nil
}

func (c *Client) createResponse(ctx context.Context, updateID int64, attempt int, instructions string, items []responses.ResponseInputItemUnionParam, tools []responses.ToolUnionParam) (*responses.Response, error) {
	stage := observability.Start(ctx, updateID, "openai_response")
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
		requestErr := apiError(err)
		stage.Fail(ctx, openAIErrorCode(requestErr), append([]any{"attempt", attempt, "model", c.model, "reasoning_effort", string(c.reasoningEffort)}, openAIStatusFields(requestErr)...)...)
		return nil, requestErr
	}
	if response == nil {
		stage.Fail(ctx, "empty_response_envelope", "attempt", attempt, "model", c.model)
		return nil, ErrRequestFailed
	}
	stage.Complete(ctx, "attempt", attempt, "model", c.model, "reasoning_effort", string(c.reasoningEffort), "response_status", string(response.Status), "input_tokens", response.Usage.InputTokens, "output_tokens", response.Usage.OutputTokens, "output_item_count", len(response.Output), "tool_enabled", len(tools) > 0)
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

func runMemoryCall(ctx context.Context, updateID int64, callIndex int, memory domain.AgentMemory, call memoryCall) (string, error) {
	stage := observability.Start(ctx, updateID, "memory_search")
	if len(call.Arguments) > MaxMemoryArgumentsBytes {
		stage.Fail(ctx, "invalid_memory_arguments", "call_index", callIndex)
		return "", ErrInvalidRequest
	}
	var arguments struct {
		Query string `json:"query"`
	}
	decoder := json.NewDecoder(strings.NewReader(call.Arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&arguments); err != nil {
		stage.Fail(ctx, "invalid_memory_arguments", "call_index", callIndex)
		return "", ErrInvalidRequest
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		stage.Fail(ctx, "invalid_memory_arguments", "call_index", callIndex)
		return "", ErrInvalidRequest
	}
	query := strings.TrimSpace(arguments.Query)
	if query == "" || len(query) > MaxMemoryQueryBytes || utf8.RuneCountInString(query) > MaxMemoryQueryLength {
		stage.Fail(ctx, "invalid_memory_query", "call_index", callIndex)
		return "", ErrInvalidRequest
	}
	output, err := memory.Search(ctx, query)
	if err != nil {
		stage.Fail(ctx, "memory_search_failed", "call_index", callIndex)
		return "", errors.Join(ErrMemoryUnavailable, err)
	}
	if len(output) > MaxMemoryResultBytes || !json.Valid([]byte(output)) {
		stage.Fail(ctx, "invalid_memory_result", "call_index", callIndex)
		return "", ErrInvalidRequest
	}
	var result struct {
		Facts []struct {
			ID string `json:"id"`
		} `json:"facts"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		stage.Fail(ctx, "invalid_memory_result", "call_index", callIndex)
		return "", ErrInvalidRequest
	}
	factIDs := make([]string, 0, len(result.Facts))
	for _, fact := range result.Facts {
		if fact.ID != "" {
			factIDs = append(factIDs, fact.ID)
		}
	}
	stage.Complete(ctx, "call_index", callIndex, "result_count", len(result.Facts), "fact_ids", factIDs)
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

func openAIErrorCode(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "request_timeout"
	case errors.Is(err, context.Canceled):
		return "request_canceled"
	case errors.Is(err, ErrContextTooLarge):
		return "context_too_large"
	case errors.Is(err, ErrIncompleteResponse):
		return "incomplete_response"
	case errors.Is(err, ErrInvalidRequest):
		return "invalid_request"
	case errors.Is(err, ErrMemoryUnavailable):
		return "memory_unavailable"
	case errors.Is(err, ErrMemoryCallLimit):
		return "memory_call_limit_exceeded"
	case errors.Is(err, ErrEmptyResponse):
		return "empty_response"
	case errors.Is(err, ErrRequestFailed):
		return "openai_request_failed"
	default:
		return "openai_operation_failed"
	}
}

func openAIStatusFields(err error) []any {
	var status statusCodeError
	if errors.As(err, &status) && status.statusCode > 0 {
		return []any{"status_code", status.statusCode}
	}
	return nil
}

func responseStatus(response *responses.Response) string {
	if response == nil {
		return "missing"
	}
	return string(response.Status)
}

type statusCodeError struct {
	statusCode int
}

func (e statusCodeError) Error() string {
	return "HTTP status " + http.StatusText(e.statusCode)
}
