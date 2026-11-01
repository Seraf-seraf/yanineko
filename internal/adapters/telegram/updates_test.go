package telegram

import (
	"strings"
	"testing"

	"github.com/Seraf-seraf/yanineko/internal/domain"
)

func TestClassifyAcceptsPrivateMediaAndGroupCaptionMention(t *testing.T) {
	private := Update{ID: 1, Message: &Message{
		ID: 2, Chat: Chat{ID: 3, Type: "private"}, From: &User{ID: 4},
		Photo: []PhotoSize{{FileID: "photo-id", Width: 100, Height: 100}},
	}}
	incoming, accepted, reason := ClassifyWithReason(private, 99, "yanineko")
	if !accepted || reason != "private_message" || incoming.Kind != domain.MessageText || incoming.Text == "" {
		t.Fatalf("private media classification = (%#v, %t, %q)", incoming, accepted, reason)
	}

	group := Update{ID: 5, Message: &Message{
		ID: 6, Chat: Chat{ID: -10, Type: "supergroup"}, From: &User{ID: 4},
		Caption: "@yanineko что на этой картинке?", Photo: []PhotoSize{{FileID: "photo-id"}},
	}}
	if !NeedsBotUsername(group) {
		t.Fatal("caption mention must trigger bot username lookup")
	}
	incoming, accepted, reason = ClassifyWithReason(group, 99, "yanineko")
	if !accepted || reason != "group_mention" || incoming.Text != group.Message.Caption {
		t.Fatalf("group caption media classification = (%#v, %t, %q)", incoming, accepted, reason)
	}
}

func TestClassifyAcceptsImageDocumentWhenMIMETypeIsMissing(t *testing.T) {
	update := Update{ID: 7, Message: &Message{
		ID: 8, Chat: Chat{ID: 9, Type: "private"}, From: &User{ID: 10},
		Document: &MediaFile{FileID: "image-file", FileName: "photo.PNG"},
	}}
	incoming, accepted, reason := ClassifyWithReason(update, 99, "yanineko")
	if !accepted || reason != "private_message" || incoming.Text == "" {
		t.Fatalf("image document classification = (%#v, %t, %q), want accepted private media", incoming, accepted, reason)
	}
	refs := mediaReferences(update.Message)
	if len(refs) != 1 || refs[0].fileID != "image-file" || refs[0].kind != "image" {
		t.Fatalf("image document media references = %#v, want image file", refs)
	}
}

func TestClassifyIgnoresDocumentWithoutImageHints(t *testing.T) {
	update := Update{ID: 11, Message: &Message{
		ID: 12, Chat: Chat{ID: 13, Type: "private"}, From: &User{ID: 14},
		Document: &MediaFile{FileID: "untyped-file"},
	}}
	_, accepted, reason := ClassifyWithReason(update, 99, "yanineko")
	if accepted || reason != "empty_text" {
		t.Fatalf("untyped document classification = (%t, %q), want ignored empty_text", accepted, reason)
	}
}

func TestClassifyAcceptsReplyToBotAboutRepliedMedia(t *testing.T) {
	update := Update{ID: 10, Message: &Message{
		ID: 11, Chat: Chat{ID: -20, Type: "group"}, From: &User{ID: 12}, Text: "Что здесь происходит?",
		ReplyToMessage: &Message{
			ID: 9, Chat: Chat{ID: -20, Type: "group"}, From: &User{ID: 99, IsBot: true},
			Animation: &MediaFile{FileID: "reply-gif", MIMEType: "video/mp4"},
		},
	}}
	incoming, accepted, reason := ClassifyWithReason(update, 99, "yanineko")
	if !accepted || reason != "group_reply" || incoming.Text != update.Message.Text {
		t.Fatalf("group reply classification = (%#v, %t, %q)", incoming, accepted, reason)
	}
	refs := mediaReferences(update.Message.ReplyToMessage)
	if len(refs) != 1 || refs[0].fileID != "reply-gif" || refs[0].kind != "animation" {
		t.Fatalf("reply media references = %#v, want replied animation", refs)
	}
}

func TestClassifyIgnoresUnaddressedGroupSticker(t *testing.T) {
	update := Update{ID: 20, Message: &Message{
		ID: 21, Chat: Chat{ID: -30, Type: "group"}, From: &User{ID: 22},
		Sticker: &Sticker{FileID: "sticker-id"},
	}}
	_, accepted, reason := ClassifyWithReason(update, 99, "yanineko")
	if accepted || reason != "group_not_addressed" {
		t.Fatalf("unaddressed group sticker classification = (%t, %q), want ignored", accepted, reason)
	}
	if !strings.Contains(update.Message.Sticker.FileID, "sticker") {
		t.Fatal("test sticker setup is invalid")
	}
}
