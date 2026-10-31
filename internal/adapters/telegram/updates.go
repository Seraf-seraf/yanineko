package telegram

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/Seraf-seraf/yanineko/internal/domain"
)

type Update struct {
	ID      int64
	Message *Message
}

type Message struct {
	ID             int64           `json:"message_id"`
	Chat           Chat            `json:"chat"`
	From           *User           `json:"from"`
	SenderChat     *Chat           `json:"sender_chat"`
	Text           string          `json:"text"`
	ThreadID       int64           `json:"message_thread_id"`
	Entities       []Entity        `json:"entities"`
	ReplyToMessage *ReplyToMessage `json:"reply_to_message"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type User struct {
	ID    int64 `json:"id"`
	IsBot bool  `json:"is_bot"`
}

type Entity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

type ReplyToMessage struct {
	From *User `json:"from"`
}

func ParseUpdate(reader io.Reader) (Update, bool, error) {
	decoder := json.NewDecoder(reader)
	var envelope map[string]json.RawMessage
	if err := decoder.Decode(&envelope); err != nil {
		return Update{}, false, err
	}
	if envelope == nil {
		return Update{}, false, errors.New("update must be a JSON object")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Update{}, false, errors.New("multiple JSON values")
		}
		return Update{}, false, err
	}

	updateIDRaw, ok := envelope["update_id"]
	if !ok {
		return Update{}, false, errors.New("missing update_id")
	}
	var updateID int64
	if err := json.Unmarshal(updateIDRaw, &updateID); err != nil || updateID < 0 {
		return Update{}, false, errors.New("invalid update_id")
	}

	eventCount := 0
	for key, raw := range envelope {
		if key == "update_id" || string(raw) == "null" {
			continue
		}
		eventCount++
	}
	if eventCount != 1 {
		return Update{}, false, errors.New("update must contain exactly one event")
	}

	messageRaw, hasMessage := envelope["message"]
	if !hasMessage || string(messageRaw) == "null" {
		return Update{ID: updateID}, false, nil
	}
	var message Message
	if err := json.Unmarshal(messageRaw, &message); err != nil {
		return Update{}, false, err
	}
	if message.ID <= 0 || message.Chat.ID == 0 || (message.ThreadID < 0) {
		return Update{}, false, errors.New("invalid message fields")
	}
	return Update{ID: updateID, Message: &message}, true, nil
}

func NeedsBotUsername(update Update) bool {
	message := update.Message
	if message == nil || message.Text == "" || message.From == nil || message.From.IsBot || message.SenderChat != nil {
		return false
	}
	_, commandTarget, hasCommand := parseCommand(message)
	if hasCommand && commandTarget != "" {
		return true
	}
	return (message.Chat.Type == "group" || message.Chat.Type == "supergroup") && hasMentionEntity(message)
}

func Classify(update Update, botID int64, botUsername string) (domain.Incoming, bool) {
	message := update.Message
	if message == nil || message.From == nil || message.From.ID <= 0 || message.From.IsBot || message.SenderChat != nil || strings.TrimSpace(message.Text) == "" {
		return domain.Incoming{}, false
	}
	private := message.Chat.Type == "private"
	group := message.Chat.Type == "group" || message.Chat.Type == "supergroup"
	if !private && !group {
		return domain.Incoming{}, false
	}

	command, commandTarget, hasCommand := parseCommand(message)
	if hasCommand {
		if commandTarget != "" && !strings.EqualFold(commandTarget, botUsername) {
			return domain.Incoming{}, false
		}
		if !strings.EqualFold(command, "about") {
			return domain.Incoming{}, false
		}
		if group && commandTarget == "" && !isReplyToBot(message, botID) && !hasMention(message, botUsername) {
			return domain.Incoming{}, false
		}
		return incoming(update, message, domain.MessageAbout), true
	}

	if private {
		return incoming(update, message, domain.MessageText), true
	}
	if isReplyToBot(message, botID) || hasMention(message, botUsername) {
		return incoming(update, message, domain.MessageText), true
	}
	return domain.Incoming{}, false
}

func incoming(update Update, message *Message, kind domain.MessageKind) domain.Incoming {
	return domain.Incoming{
		UpdateID:  update.ID,
		Key:       domain.ConversationKey{ChatID: message.Chat.ID, ThreadID: message.ThreadID},
		MessageID: message.ID,
		Text:      message.Text,
		Kind:      kind,
	}
}

func parseCommand(message *Message) (string, string, bool) {
	for _, entity := range message.Entities {
		if entity.Type != "bot_command" || entity.Offset != 0 {
			continue
		}
		value, ok := entityText(message.Text, entity.Offset, entity.Length)
		if !ok || !strings.HasPrefix(value, "/") {
			return "", "", false
		}
		command := value[1:]
		if at := strings.IndexByte(command, '@'); at >= 0 {
			if strings.IndexByte(command[at+1:], '@') >= 0 || at == len(command)-1 {
				return "", "", false
			}
			return command[:at], command[at+1:], true
		}
		return command, "", true
	}
	return "", "", false
}

func hasMentionEntity(message *Message) bool {
	for _, entity := range message.Entities {
		if entity.Type == "mention" {
			return true
		}
	}
	return false
}

func hasMention(message *Message, botUsername string) bool {
	if botUsername == "" {
		return false
	}
	for _, entity := range message.Entities {
		if entity.Type != "mention" {
			continue
		}
		value, ok := entityText(message.Text, entity.Offset, entity.Length)
		if ok && strings.HasPrefix(value, "@") && strings.EqualFold(value[1:], botUsername) {
			return true
		}
	}
	return false
}

func isReplyToBot(message *Message, botID int64) bool {
	return botID > 0 && message.ReplyToMessage != nil && message.ReplyToMessage.From != nil && message.ReplyToMessage.From.ID == botID
}

func entityText(text string, offset, length int) (string, bool) {
	if offset < 0 || length <= 0 || offset > len(text)*2 || length > len(text)*2-offset {
		return "", false
	}
	runes := []rune(text)
	end := offset + length
	startRune, endRune := -1, -1
	units := 0
	for index, character := range runes {
		if units == offset {
			startRune = index
		}
		if units == end {
			endRune = index
		}
		if character > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	if units == offset {
		startRune = len(runes)
	}
	if units == end {
		endRune = len(runes)
	}
	if startRune < 0 || endRune < startRune {
		return "", false
	}
	return string(runes[startRune:endRune]), true
}
