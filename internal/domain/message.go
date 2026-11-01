package domain

const MaxOutgoingTextRunes = 4096

type ConversationKey struct {
	ChatID   int64
	ThreadID int64
}

type MessageKind uint8

const (
	MessageText MessageKind = iota + 1
	MessageAbout
)

type Incoming struct {
	UpdateID  int64
	Key       ConversationKey
	MessageID int64
	Text      string
	Kind      MessageKind
}

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type Message struct {
	UpdateID     int64
	ArchiveOrder int64
	Role         Role
	Text         string
}

type Outgoing struct {
	Key              ConversationKey
	ReplyToMessageID int64
	Text             string
}
