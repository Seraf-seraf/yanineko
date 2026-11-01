package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Seraf-seraf/yanineko/internal/domain"
)

const RequestTimeout = 6 * time.Second
const MaxResponseBytes int64 = 1 << 20
const MaxMessageRunes = domain.MaxOutgoingTextRunes

var ErrInvalidTelegramRequest = errors.New("invalid Telegram request")
var ErrTelegramRequest = errors.New("Telegram request failed")
var ErrTelegramAPI = errors.New("Telegram API returned an error")
var ErrTelegramResponse = errors.New("invalid Telegram API response")
var ErrUnexpectedBotIdentity = errors.New("Telegram token identity mismatch")

type Client struct {
	token         string
	expectedBotID int64
	baseURL       string
	httpClient    *http.Client
}

type replyParameters struct {
	MessageID                int64 `json:"message_id"`
	AllowSendingWithoutReply bool  `json:"allow_sending_without_reply"`
}

type sendMessageRequest struct {
	ChatID          int64           `json:"chat_id"`
	MessageThreadID int64           `json:"message_thread_id,omitempty"`
	Text            string          `json:"text"`
	ReplyParameters replyParameters `json:"reply_parameters"`
}

type apiEnvelope struct {
	OK        bool            `json:"ok"`
	Result    json.RawMessage `json:"result"`
	ErrorCode int             `json:"error_code"`
}

type botProfile struct {
	ID       int64  `json:"id"`
	IsBot    bool   `json:"is_bot"`
	Username string `json:"username"`
}

type sentMessage struct {
	MessageID int64 `json:"message_id"`
}

type apiStatusError struct {
	statusCode int
}

func NewClient(token string, expectedBotID int64) (*Client, error) {
	if !validBotToken(token, expectedBotID) {
		return nil, ErrInvalidTelegramRequest
	}
	httpClient := &http.Client{
		Timeout: RequestTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return newClient(token, expectedBotID, "https://api.telegram.org", httpClient), nil
}

func newClient(token string, expectedBotID int64, baseURL string, httpClient *http.Client) *Client {
	return &Client{
		token:         token,
		expectedBotID: expectedBotID,
		baseURL:       strings.TrimRight(baseURL, "/"),
		httpClient:    httpClient,
	}
}

func (c *Client) GetMe(ctx context.Context) (string, error) {
	var profile botProfile
	if err := c.call(ctx, "getMe", nil, &profile); err != nil {
		return "", err
	}
	if profile.ID != c.expectedBotID || !profile.IsBot || !validUsername(profile.Username) {
		return "", ErrUnexpectedBotIdentity
	}
	return profile.Username, nil
}

func (c *Client) Send(ctx context.Context, outgoing domain.Outgoing) (int64, error) {
	if c == nil || outgoing.Key.ChatID == 0 || outgoing.Key.ThreadID < 0 || outgoing.ReplyToMessageID <= 0 ||
		!utf8.ValidString(outgoing.Text) || strings.TrimSpace(outgoing.Text) == "" || utf8.RuneCountInString(outgoing.Text) > MaxMessageRunes {
		return 0, ErrInvalidTelegramRequest
	}

	request := sendMessageRequest{
		ChatID: outgoing.Key.ChatID,
		Text:   outgoing.Text,
		ReplyParameters: replyParameters{
			MessageID:                outgoing.ReplyToMessageID,
			AllowSendingWithoutReply: true,
		},
	}
	if outgoing.Key.ThreadID > 0 {
		request.MessageThreadID = outgoing.Key.ThreadID
	}
	var sent sentMessage
	if err := c.call(ctx, "sendMessage", request, &sent); err != nil {
		return 0, err
	}
	if sent.MessageID <= 0 {
		return 0, ErrTelegramResponse
	}
	return sent.MessageID, nil
}

func (c *Client) call(ctx context.Context, method string, requestBody any, result any) error {
	return c.callWithTimeout(ctx, method, requestBody, result, RequestTimeout)
}

func (c *Client) callWithTimeout(ctx context.Context, method string, requestBody any, result any, timeout time.Duration) error {
	if c == nil || c.httpClient == nil || c.baseURL == "" || !validBotToken(c.token, c.expectedBotID) ||
		(method != "getMe" && method != "sendMessage" && method != "getFile") {
		return ErrInvalidTelegramRequest
	}

	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return ErrInvalidTelegramRequest
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, c.baseURL+"/bot"+c.token+"/"+method, body)
	if err != nil {
		return ErrInvalidTelegramRequest
	}
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		if requestContext.Err() != nil {
			return requestContext.Err()
		}
		return ErrTelegramRequest
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil || int64(len(responseBody)) > MaxResponseBytes {
		return ErrTelegramResponse
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return errors.Join(ErrTelegramAPI, apiStatusError{statusCode: response.StatusCode})
	}

	var envelope apiEnvelope
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return ErrTelegramResponse
	}
	if !envelope.OK {
		return errors.Join(ErrTelegramAPI, apiStatusError{statusCode: envelope.ErrorCode})
	}
	if len(envelope.Result) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Result), []byte("null")) {
		return ErrTelegramResponse
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return ErrTelegramResponse
	}
	return nil
}

func validBotToken(token string, expectedBotID int64) bool {
	if token == "" || len(token) > 256 || expectedBotID <= 0 || strings.TrimSpace(token) != token {
		return false
	}
	separator := strings.IndexByte(token, ':')
	if separator <= 0 || separator == len(token)-1 || strings.IndexByte(token[separator+1:], ':') >= 0 {
		return false
	}
	botID, err := strconv.ParseInt(token[:separator], 10, 64)
	if err != nil || botID != expectedBotID {
		return false
	}
	for _, character := range token[separator+1:] {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-') {
			return false
		}
	}
	return true
}

func validUsername(username string) bool {
	if len(username) < 5 || len(username) > 32 {
		return false
	}
	for _, character := range username {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_') {
			return false
		}
	}
	return true
}

func (e apiStatusError) Error() string {
	return "Telegram API status " + strconv.Itoa(e.statusCode)
}
