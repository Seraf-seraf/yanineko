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
	UpdateID         int64
	Key              ConversationKey
	MessageID        int64
	Text             string
	Kind             MessageKind
	Author           *Author
	Images           []Image
	MediaUnavailable bool
}

// Author identifies a Telegram user independently of the conversation key.
type Author struct {
	ID       int64
	Username string
	Name     string
	Order    int64
}

// Image is transient media prepared for visual description. It is never stored.
type Image struct {
	Data     []byte
	MIMEType string
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
	Author       *Author
}

type Outgoing struct {
	Key              ConversationKey
	ReplyToMessageID int64
	Text             string
}
