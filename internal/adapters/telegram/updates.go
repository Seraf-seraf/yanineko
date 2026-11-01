package telegram

import (
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Seraf-seraf/yanineko/internal/domain"
)

type Update struct {
	ID      int64
	Message *Message
}

type Message struct {
	ID              int64       `json:"message_id"`
	Chat            Chat        `json:"chat"`
	From            *User       `json:"from"`
	SenderChat      *Chat       `json:"sender_chat"`
	Text            string      `json:"text"`
	Caption         string      `json:"caption"`
	ThreadID        int64       `json:"message_thread_id"`
	Entities        []Entity    `json:"entities"`
	CaptionEntities []Entity    `json:"caption_entities"`
	Photo           []PhotoSize `json:"photo"`
	Animation       *MediaFile  `json:"animation"`
	Document        *MediaFile  `json:"document"`
	Sticker         *Sticker    `json:"sticker"`
	ReplyToMessage  *Message    `json:"reply_to_message"`
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

type PhotoSize struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FileSize     int64  `json:"file_size"`
}

type MediaFile struct {
	FileID       string     `json:"file_id"`
	FileUniqueID string     `json:"file_unique_id"`
	FileName     string     `json:"file_name"`
	FileSize     int64      `json:"file_size"`
	MIMEType     string     `json:"mime_type"`
	Thumbnail    *PhotoSize `json:"thumbnail"`
}

type Sticker struct {
	FileID       string     `json:"file_id"`
	FileUniqueID string     `json:"file_unique_id"`
	FileSize     int64      `json:"file_size"`
	IsAnimated   bool       `json:"is_animated"`
	IsVideo      bool       `json:"is_video"`
	Thumbnail    *PhotoSize `json:"thumbnail"`
}

type ReplyToMessage = Message

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
	if message == nil || message.From == nil || message.From.IsBot || message.SenderChat != nil {
		return false
	}
	_, commandTarget, hasCommand := parseCommand(message)
	if hasCommand && commandTarget != "" {
		return true
	}
	return (message.Chat.Type == "group" || message.Chat.Type == "supergroup") &&
		(hasMentionEntity(message) || strings.ContainsRune(message.Text, '@') || strings.ContainsRune(message.Caption, '@'))
}

func Classify(update Update, botID int64, botUsername string) (domain.Incoming, bool) {
	incoming, accepted, _ := ClassifyWithReason(update, botID, botUsername)
	return incoming, accepted
}

func ClassifyWithReason(update Update, botID int64, botUsername string) (domain.Incoming, bool, string) {
	message := update.Message
	if message == nil {
		return domain.Incoming{}, false, "no_message"
	}
	if message.From == nil || message.From.ID <= 0 || message.From.IsBot || message.SenderChat != nil {
		return domain.Incoming{}, false, "ineligible_sender"
	}
	text := messageText(message)
	if strings.TrimSpace(text) == "" && !hasMedia(message) {
		return domain.Incoming{}, false, "empty_text"
	}
	if strings.TrimSpace(text) == "" {
		text = "Пользователь отправил изображение или анимацию."
	}
	private := message.Chat.Type == "private"
	group := message.Chat.Type == "group" || message.Chat.Type == "supergroup"
	if !private && !group {
		return domain.Incoming{}, false, "unsupported_chat_type"
	}

	command, commandTarget, hasCommand := parseCommand(message)
	if hasCommand {
		if commandTarget != "" && !strings.EqualFold(commandTarget, botUsername) {
			return domain.Incoming{}, false, "command_for_other_bot"
		}
		if !strings.EqualFold(command, "about") {
			return domain.Incoming{}, false, "unsupported_command"
		}
		if group && commandTarget == "" && !isReplyToBot(message, botID) && !hasMention(message, botUsername) {
			return domain.Incoming{}, false, "group_command_not_addressed"
		}
		return incoming(update, message, domain.MessageAbout), true, "about_command"
	}

	if private {
		return incoming(update, message, domain.MessageText), true, "private_message"
	}
	if isReplyToBot(message, botID) {
		return incoming(update, message, domain.MessageText), true, "group_reply"
	}
	if hasMention(message, botUsername) {
		return incoming(update, message, domain.MessageText), true, "group_mention"
	}
	return domain.Incoming{}, false, "group_not_addressed"
}

func incoming(update Update, message *Message, kind domain.MessageKind) domain.Incoming {
	text := messageText(message)
	if strings.TrimSpace(text) == "" && hasMedia(message) {
		text = "Пользователь отправил изображение или анимацию."
	}
	return domain.Incoming{
		UpdateID:  update.ID,
		Key:       domain.ConversationKey{ChatID: message.Chat.ID, ThreadID: message.ThreadID},
		MessageID: message.ID,
		Text:      text,
		Kind:      kind,
	}
}

func messageText(message *Message) string {
	if strings.TrimSpace(message.Text) != "" {
		return message.Text
	}
	return message.Caption
}

func hasMedia(message *Message) bool {
	return len(message.Photo) > 0 || message.Animation != nil || isImageDocument(message.Document) || message.Sticker != nil
}

func isImageDocument(document *MediaFile) bool {
	if document == nil {
		return false
	}
	if strings.HasPrefix(strings.ToLower(document.MIMEType), "image/") {
		return true
	}
	switch strings.ToLower(path.Ext(document.FileName)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif":
		return true
	default:
		return false
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
	for _, entity := range append(append([]Entity(nil), message.Entities...), message.CaptionEntities...) {
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
	text, entities := message.Text, message.Entities
	if text == "" {
		text, entities = message.Caption, message.CaptionEntities
	}
	for _, entity := range entities {
		if entity.Type != "mention" {
			continue
		}
		value, ok := entityText(text, entity.Offset, entity.Length)
		if ok && strings.HasPrefix(value, "@") && strings.EqualFold(value[1:], botUsername) {
			return true
		}
	}
	return hasTextMention(text, botUsername)
}

func hasTextMention(text, botUsername string) bool {
	if botUsername == "" {
		return false
	}

	mentionLength := len(botUsername) + 1
	for offset := 0; offset < len(text); {
		at := strings.IndexByte(text[offset:], '@')
		if at < 0 {
			return false
		}
		start := offset + at
		end := start + mentionLength
		if end <= len(text) && strings.EqualFold(text[start+1:end], botUsername) &&
			mentionBoundaryBefore(text, start) && mentionBoundaryAfter(text, end) {
			return true
		}
		offset = start + 1
	}
	return false
}

func mentionBoundaryBefore(text string, index int) bool {
	if index == 0 {
		return true
	}
	previous, _ := utf8.DecodeLastRuneInString(text[:index])
	return !isMentionWordRune(previous)
}

func mentionBoundaryAfter(text string, index int) bool {
	if index == len(text) {
		return true
	}
	next, _ := utf8.DecodeRuneInString(text[index:])
	return !isMentionWordRune(next)
}

func isMentionWordRune(value rune) bool {
	return value == '@' || value == '_' || unicode.IsLetter(value) || unicode.IsDigit(value)
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
