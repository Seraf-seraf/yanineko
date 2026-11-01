package config

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

type Config struct {
	TelegramBotToken      string
	TelegramBotID         int64
	TelegramWebhookSecret string
	OpenAIAPIKey          string
	OpenAIModel           string
	OpenAIReasoningEffort string
	SystemPrompt          string
	OpenAIBaseURL         string
	TursoDatabaseURL      string
	TursoAuthToken        string
	DatabaseURL           string
}

const MaxSystemPromptBytes = 64 * 1024
const DefaultOpenAIModel = "gpt-6-luna"

func Load(getenv func(string) string) (Config, error) {
	if getenv == nil {
		return Config{}, errors.New("environment reader is required")
	}

	config := Config{
		TelegramBotToken:      getenv("TELEGRAM_BOT_TOKEN"),
		TelegramWebhookSecret: getenv("TELEGRAM_WEBHOOK_SECRET"),
		OpenAIAPIKey:          getenv("OPENAI_API_KEY"),
		OpenAIModel:           getenv("OPENAI_MODEL"),
		OpenAIReasoningEffort: getenv("OPENAI_REASONING_EFFORT"),
		SystemPrompt:          getenv("SYSTEM_PROMPT"),
		OpenAIBaseURL:         getenv("OPENAI_BASE_URL"),
		TursoDatabaseURL:      getenv("TURSO_DATABASE_URL"),
		TursoAuthToken:        getenv("TURSO_AUTH_TOKEN"),
		DatabaseURL:           getenv("DATABASE_URL"),
	}
	if config.OpenAIModel == "" {
		config.OpenAIModel = DefaultOpenAIModel
	}

	botID, err := telegramBotID(config.TelegramBotToken)
	if err != nil {
		return Config{}, err
	}
	config.TelegramBotID = botID

	if !validWebhookSecret(config.TelegramWebhookSecret) {
		return Config{}, errors.New("invalid TELEGRAM_WEBHOOK_SECRET")
	}
	if !validSingleLineValue(config.OpenAIAPIKey) {
		return Config{}, errors.New("invalid OPENAI_API_KEY")
	}
	if !validSingleLineValue(config.OpenAIModel) {
		return Config{}, errors.New("invalid OPENAI_MODEL")
	}
	if config.OpenAIReasoningEffort == "" {
		config.OpenAIReasoningEffort = "low"
	}
	if !validReasoningEffort(config.OpenAIReasoningEffort) {
		return Config{}, errors.New("invalid OPENAI_REASONING_EFFORT")
	}
	if strings.TrimSpace(config.SystemPrompt) == "" || strings.ContainsRune(config.SystemPrompt, '\x00') || len(config.SystemPrompt) > MaxSystemPromptBytes {
		return Config{}, errors.New("invalid SYSTEM_PROMPT")
	}
	if config.OpenAIBaseURL != "" && !validBaseURL(config.OpenAIBaseURL) {
		return Config{}, errors.New("invalid OPENAI_BASE_URL")
	}
	if !validTursoURL(config.TursoDatabaseURL) {
		return Config{}, errors.New("invalid TURSO_DATABASE_URL")
	}
	if !validSingleLineValue(config.TursoAuthToken) || len(config.TursoAuthToken) > 4096 {
		return Config{}, errors.New("invalid TURSO_AUTH_TOKEN")
	}
	if config.DatabaseURL != "" && !validDatabaseURL(config.DatabaseURL) {
		return Config{}, errors.New("invalid DATABASE_URL")
	}

	return config, nil
}

func validReasoningEffort(value string) bool {
	switch value {
	case "none", "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}

func validTursoURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" || parsed.Opaque != "" || (parsed.Path != "" && parsed.Path != "/") {
		return false
	}
	switch parsed.Scheme {
	case "turso", "libsql", "https":
		return true
	default:
		return false
	}
}

func telegramBotID(token string) (int64, error) {
	if len(token) > 256 || strings.TrimSpace(token) != token {
		return 0, errors.New("invalid TELEGRAM_BOT_TOKEN")
	}
	separator := strings.IndexByte(token, ':')
	if separator < 1 || separator == len(token)-1 || strings.Contains(token[separator+1:], ":") {
		return 0, errors.New("invalid TELEGRAM_BOT_TOKEN")
	}
	for _, character := range token[separator+1:] {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-') {
			return 0, errors.New("invalid TELEGRAM_BOT_TOKEN")
		}
	}
	botID, err := strconv.ParseInt(token[:separator], 10, 64)
	if err != nil || botID <= 0 {
		return 0, errors.New("invalid TELEGRAM_BOT_TOKEN")
	}
	return botID, nil
}

func validWebhookSecret(secret string) bool {
	if len(secret) < 1 || len(secret) > 256 {
		return false
	}
	for _, character := range secret {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-') {
			return false
		}
	}
	return true
}

func validSingleLineValue(value string) bool {
	if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < '!' || character > '~' {
			return false
		}
	}
	return true
}

func validBaseURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Hostname() != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.Opaque == ""
}

func validDatabaseURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Hostname() == "" || parsed.User == nil || parsed.User.Username() == "" || parsed.Fragment != "" || parsed.Opaque != "" || parsed.Path == "" || parsed.Path == "/" {
		return false
	}
	_, hasPassword := parsed.User.Password()
	if !hasPassword {
		return false
	}
	if isLoopbackHost(parsed.Hostname()) {
		return true
	}
	switch parsed.Query().Get("sslmode") {
	case "require", "verify-ca", "verify-full":
		return true
	default:
		return false
	}
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
