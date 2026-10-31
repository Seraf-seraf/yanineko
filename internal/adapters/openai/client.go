package openaiadapter

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/Seraf-seraf/yanineko/internal/domain"
)

const RequestTimeout = 30 * time.Second
const MaxRequestContentBytes = 96 * 1024
const MaxOutputTokens int64 = 1024

var ErrInvalidRequest = errors.New("invalid OpenAI request")
var ErrRequestFailed = errors.New("OpenAI request failed")
var ErrIncompleteResponse = errors.New("OpenAI response was incomplete")
var ErrEmptyResponse = errors.New("OpenAI response contained no text")

type Client struct {
	sdk   openai.Client
	model string
}

func New(apiKey, model, baseURL string) *Client {
	return newClient(apiKey, model, baseURL, nil)
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
	return &Client{sdk: openai.NewClient(options...), model: model}
}

func (c *Client) Respond(ctx context.Context, instructions string, history []domain.Message) (string, error) {
	if c == nil || c.model == "" || strings.TrimSpace(instructions) == "" || len(history) == 0 {
		return "", ErrInvalidRequest
	}

	contentBytes := len(instructions)
	input := make(responses.ResponseInputParam, 0, len(history))
	for _, message := range history {
		if message.Role != domain.RoleUser && message.Role != domain.RoleAssistant || strings.TrimSpace(message.Text) == "" {
			return "", ErrInvalidRequest
		}
		contentBytes += len(message.Text)
		input = append(input, responses.ResponseInputItemParamOfMessage(message.Text, responses.EasyInputMessageRole(message.Role)))
	}
	if contentBytes > MaxRequestContentBytes {
		return "", ErrInvalidRequest
	}

	requestContext, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	response, err := c.sdk.Responses.New(requestContext, responses.ResponseNewParams{
		Instructions:    param.NewOpt(instructions),
		Input:           responses.ResponseNewParamsInputUnion{OfInputItemList: input},
		MaxOutputTokens: param.NewOpt(MaxOutputTokens),
		Model:           shared.ResponsesModel(c.model),
		Store:           param.NewOpt(false),
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		var apiError *openai.Error
		if errors.As(err, &apiError) {
			return "", errors.Join(ErrRequestFailed, statusCodeError{statusCode: apiError.StatusCode})
		}
		return "", ErrRequestFailed
	}
	if response == nil || response.Status != responses.ResponseStatusCompleted {
		return "", ErrIncompleteResponse
	}
	text := strings.TrimSpace(response.OutputText())
	if text == "" {
		return "", ErrEmptyResponse
	}
	return text, nil
}

type statusCodeError struct {
	statusCode int
}

func (e statusCodeError) Error() string {
	return "HTTP status " + http.StatusText(e.statusCode)
}
