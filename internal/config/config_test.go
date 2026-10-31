package config

import (
	"strings"
	"testing"
)

func TestLoadAcceptsValidConfigurationAndPreservesMultilinePrompt(t *testing.T) {
	t.Parallel()

	values := validValues()
	values["SYSTEM_PROMPT"] = "Первая строка\nВторая строка"
	values["OPENAI_BASE_URL"] = "https://api.openai.com/v1"
	config, err := Load(lookup(values))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.TelegramBotID != 123456789 {
		t.Fatalf("TelegramBotID = %d, want 123456789", config.TelegramBotID)
	}
	if config.SystemPrompt != values["SYSTEM_PROMPT"] {
		t.Fatalf("SystemPrompt = %q, want exact multiline value", config.SystemPrompt)
	}
	if config.OpenAIBaseURL != values["OPENAI_BASE_URL"] {
		t.Fatalf("OpenAIBaseURL = %q, want %q", config.OpenAIBaseURL, values["OPENAI_BASE_URL"])
	}
}

func TestLoadAcceptsOptionalBaseURLWhenUnset(t *testing.T) {
	t.Parallel()

	values := validValues()
	config, err := Load(lookup(values))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.OpenAIBaseURL != "" {
		t.Fatalf("OpenAIBaseURL = %q, want empty", config.OpenAIBaseURL)
	}
}

func TestLoadRejectsMissingAndMalformedValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "missing token", key: "TELEGRAM_BOT_TOKEN", value: ""},
		{name: "invalid token", key: "TELEGRAM_BOT_TOKEN", value: "not-a-token"},
		{name: "unsafe token path", key: "TELEGRAM_BOT_TOKEN", value: "123456789:token/other"},
		{name: "invalid webhook secret", key: "TELEGRAM_WEBHOOK_SECRET", value: "contains spaces"},
		{name: "missing API key", key: "OPENAI_API_KEY", value: ""},
		{name: "multiline model", key: "OPENAI_MODEL", value: "model\nother"},
		{name: "blank prompt", key: "SYSTEM_PROMPT", value: " \n "},
		{name: "insecure base URL", key: "OPENAI_BASE_URL", value: "http://api.openai.com/v1"},
		{name: "invalid Turso URL", key: "TURSO_DATABASE_URL", value: "https://"},
		{name: "missing Turso token", key: "TURSO_AUTH_TOKEN", value: ""},
		{name: "invalid database URL", key: "DATABASE_URL", value: "https://db.example"},
		{name: "remote database without TLS", key: "DATABASE_URL", value: "postgresql://user:password@db.example/yanineko"},
		{name: "oversized prompt", key: "SYSTEM_PROMPT", value: strings.Repeat("x", MaxSystemPromptBytes+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			values := validValues()
			values[test.key] = test.value
			_, err := Load(lookup(values))
			if err == nil {
				t.Fatal("Load() error = nil, want validation error")
			}
			if strings.Contains(err.Error(), values["TELEGRAM_BOT_TOKEN"]) && values["TELEGRAM_BOT_TOKEN"] != "" {
				t.Fatalf("validation error leaked a secret: %v", err)
			}
		})
	}
}

func TestLoadRejectsNilEnvironmentReader(t *testing.T) {
	t.Parallel()

	if _, err := Load(nil); err == nil {
		t.Fatal("Load(nil) error = nil, want error")
	}
}

func validValues() map[string]string {
	return map[string]string{
		"TELEGRAM_BOT_TOKEN":      "123456789:synthetic-token",
		"TELEGRAM_WEBHOOK_SECRET": "synthetic_secret-123",
		"OPENAI_API_KEY":          "synthetic-openai-key",
		"OPENAI_MODEL":            "gpt-5.1-mini",
		"SYSTEM_PROMPT":           "Действуй как персонаж.",
		"TURSO_DATABASE_URL":      "libsql://yanineko-test.turso.io",
		"TURSO_AUTH_TOKEN":        "synthetic-turso-token",
		"DATABASE_URL":            "postgresql://user:password@localhost/yanineko?sslmode=disable",
	}
}

func lookup(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}
