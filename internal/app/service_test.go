package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Seraf-seraf/yanineko/internal/domain"
)

func TestHandleAboutDoesNotCallResponder(t *testing.T) {
	t.Parallel()

	input := sampleIncoming(domain.MessageAbout, "/about")
	store := newTestStore(input)
	responder := &testResponder{err: errors.New("unexpected response call")}
	messenger := &testMessenger{}
	service := Service{SystemPrompt: "private character prompt", Store: store, Responder: responder, Messenger: messenger}

	if err := service.Handle(context.Background(), input); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if responder.calls != 0 {
		t.Fatalf("Responder calls = %d, want 0", responder.calls)
	}
	if store.aboutResponse != AboutMessage {
		t.Fatalf("stored about response = %q, want static text", store.aboutResponse)
	}
	if len(messenger.sent) != 1 || messenger.sent[0].Text != AboutMessage {
		t.Fatalf("sent messages = %#v, want one static about reply", messenger.sent)
	}
	if messenger.sent[0].ReplyToMessageID != input.MessageID {
		t.Fatalf("reply_to_message_id = %d, want %d", messenger.sent[0].ReplyToMessageID, input.MessageID)
	}
	if !store.sentMarked {
		t.Fatal("confirmed send was not persisted")
	}
}

func TestHandleTextPassesPromptAndHistoryAndPersistsResponse(t *testing.T) {
	t.Parallel()

	input := sampleIncoming(domain.MessageText, "Привет, @yanineko")
	store := newTestStore(input)
	store.history = []domain.Message{{Role: domain.RoleUser, Text: "Как дела?"}, {Role: domain.RoleAssistant, Text: "Хорошо."}}
	responder := &testResponder{response: "  Привет!  "}
	messenger := &testMessenger{}
	service := Service{SystemPrompt: "Точный\nмногострочный промпт", Store: store, Responder: responder, Messenger: messenger}

	if err := service.Handle(context.Background(), input); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if responder.prompt != service.SystemPrompt {
		t.Fatalf("prompt = %q, want exact configured prompt", responder.prompt)
	}
	wantHistory := []domain.Message{
		{Role: domain.RoleUser, Text: "Как дела?"},
		{Role: domain.RoleAssistant, Text: "Хорошо."},
		{Role: domain.RoleUser, Text: input.Text},
	}
	if !equalMessages(responder.history, wantHistory) {
		t.Fatalf("history = %#v, want %#v", responder.history, wantHistory)
	}
	if store.response != "Привет!" {
		t.Fatalf("stored response = %q, want trimmed text", store.response)
	}
	if len(messenger.sent) != 1 || messenger.sent[0].Text != "Привет!" {
		t.Fatalf("sent messages = %#v, want one trimmed response", messenger.sent)
	}
	if messenger.sent[0].Key != input.Key || messenger.sent[0].ReplyToMessageID != input.MessageID {
		t.Fatalf("outgoing target = %#v, want input conversation and message", messenger.sent)
	}
}

func TestHandleTruncatesTelegramResponseBeforePersistence(t *testing.T) {
	t.Parallel()

	input := sampleIncoming(domain.MessageText, "hello")
	store := newTestStore(input)
	response := strings.Repeat("猫", MaxTelegramMessageRunes+1)
	messenger := &testMessenger{}
	service := Service{
		Store: store, Responder: &testResponder{response: response}, Messenger: messenger,
	}

	if err := service.Handle(context.Background(), input); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if utf8.RuneCountInString(store.response) != MaxTelegramMessageRunes || !utf8.ValidString(store.response) {
		t.Fatalf("stored response has %d runes and valid UTF-8=%t, want %d runes and valid UTF-8", utf8.RuneCountInString(store.response), utf8.ValidString(store.response), MaxTelegramMessageRunes)
	}
	if len(messenger.sent) != 1 || messenger.sent[0].Text != store.response {
		t.Fatalf("sent message = %#v, stored response = %q; want exact stored text", messenger.sent, store.response)
	}
}

func TestHandleReleasesClaimAfterResponderError(t *testing.T) {
	t.Parallel()

	input := sampleIncoming(domain.MessageText, "hello")
	store := newTestStore(input)
	responder := &testResponder{err: errors.New("model unavailable")}
	messenger := &testMessenger{}
	service := Service{Store: store, Responder: responder, Messenger: messenger}

	err := service.Handle(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "response generation failed") {
		t.Fatalf("Handle() error = %v, want response-generation error", err)
	}
	if !store.released {
		t.Fatal("processing claim was not released after generation failure")
	}
	if len(messenger.sent) != 0 {
		t.Fatalf("sent messages = %#v, want none", messenger.sent)
	}
}

func TestHandleDoesNotBlindlyResendUnknownDelivery(t *testing.T) {
	t.Parallel()

	input := sampleIncoming(domain.MessageText, "hello")
	store := newTestStore(input)
	store.begin.Status = BeginReady
	messenger := &testMessenger{err: errors.New("timeout")}
	service := Service{Store: store, Messenger: messenger}

	err := service.Handle(context.Background(), input)
	if !errors.Is(err, ErrDeliveryUnknown) {
		t.Fatalf("Handle() error = %v, want delivery-unknown", err)
	}
	if !store.unknownMarked {
		t.Fatal("unknown delivery state was not persisted")
	}
	if len(messenger.sent) != 1 {
		t.Fatalf("send attempts = %d, want exactly one", len(messenger.sent))
	}
}

func TestHandleDuplicateSentUpdateIsAcknowledgedWithoutSideEffects(t *testing.T) {
	t.Parallel()

	input := sampleIncoming(domain.MessageText, "hello")
	store := newTestStore(input)
	store.begin.Status = BeginSent
	responder := &testResponder{}
	messenger := &testMessenger{}
	service := Service{Store: store, Responder: responder, Messenger: messenger}

	if err := service.Handle(context.Background(), input); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if responder.calls != 0 || len(messenger.sent) != 0 {
		t.Fatalf("duplicate side effects: responder=%d sends=%d", responder.calls, len(messenger.sent))
	}
}

func TestPrepareHistoryTrimsOldestCompletePairs(t *testing.T) {
	t.Parallel()

	previous := make([]domain.Message, 0, 22)
	for index := range 11 {
		previous = append(previous,
			domain.Message{Role: domain.RoleUser, Text: "u" + strings.Repeat("x", 1) + string(rune('a'+index))},
			domain.Message{Role: domain.RoleAssistant, Text: "a" + string(rune('a'+index))})
	}
	history, err := prepareHistory(previous, domain.Message{Role: domain.RoleUser, Text: "current"})
	if err != nil {
		t.Fatalf("prepareHistory() error = %v", err)
	}
	if len(history) != MaxContextMessages {
		t.Fatalf("history length = %d, want %d", len(history), MaxContextMessages)
	}
	if history[0].Text != previous[2].Text || history[len(history)-1].Text != "current" {
		t.Fatalf("trimmed history boundaries = %#v, want oldest pair removed", history)
	}
}

func TestPrepareHistoryRejectsOversizedCurrentInput(t *testing.T) {
	t.Parallel()

	_, err := prepareHistory(nil, domain.Message{Role: domain.RoleUser, Text: strings.Repeat("x", MaxConversationTextBytes+1)})
	if !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("prepareHistory() error = %v, want ErrInputTooLarge", err)
	}
}

func sampleIncoming(kind domain.MessageKind, text string) domain.Incoming {
	return domain.Incoming{
		UpdateID:  99,
		Key:       domain.ConversationKey{ChatID: -100123, ThreadID: 5},
		MessageID: 21,
		Text:      text,
		Kind:      kind,
	}
}

func equalMessages(left, right []domain.Message) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type testStore struct {
	begin         BeginResult
	history       []domain.Message
	response      string
	aboutResponse string
	released      bool
	unknownMarked bool
	sentMarked    bool
	delivery      Delivery
	claim         Claim
}

func newTestStore(incoming domain.Incoming) *testStore {
	claim := Claim{Incoming: incoming, OwnerToken: strings.Repeat("a", 32), Version: 1}
	return &testStore{
		begin: BeginResult{Status: BeginClaimed, Claim: claim},
		claim: claim,
		delivery: Delivery{
			UpdateID: incoming.UpdateID, OwnerToken: strings.Repeat("b", 32),
			ProcessingOwner: claim.OwnerToken,
			Outgoing:        domain.Outgoing{Key: incoming.Key, ReplyToMessageID: incoming.MessageID},
		},
	}
}

func (s *testStore) Begin(_ context.Context, _ domain.Incoming, duration time.Duration) (BeginResult, error) {
	if duration != ConversationLeaseDuration {
		return BeginResult{}, errors.New("unexpected conversation lease")
	}
	return s.begin, nil
}

func (s *testStore) History(_ context.Context, claim Claim) ([]domain.Message, error) {
	s.claim = claim
	return append([]domain.Message(nil), s.history...), nil
}

func (s *testStore) CommitResponse(_ context.Context, claim Claim, response string) error {
	s.claim, s.response = claim, response
	s.delivery.Outgoing.Text = response
	return nil
}

func (s *testStore) CommitAbout(_ context.Context, claim Claim, response string) error {
	s.claim, s.aboutResponse = claim, response
	s.delivery.Outgoing.Text = response
	return nil
}

func (s *testStore) ClaimDelivery(_ context.Context, updateID int64, duration time.Duration) (DeliveryResult, error) {
	if updateID != s.claim.Incoming.UpdateID || duration != DeliveryLeaseDuration {
		return DeliveryResult{}, errors.New("unexpected delivery claim")
	}
	return DeliveryResult{Status: DeliveryClaimed, Delivery: s.delivery}, nil
}

func (s *testStore) MarkSent(_ context.Context, delivery Delivery, messageID int64) error {
	if delivery.OwnerToken != s.delivery.OwnerToken || messageID <= 0 {
		return errors.New("unexpected sent marker")
	}
	s.sentMarked = true
	return nil
}

func (s *testStore) MarkDeliveryUnknown(_ context.Context, delivery Delivery) error {
	if delivery.OwnerToken != s.delivery.OwnerToken {
		return errors.New("unexpected unknown marker")
	}
	s.unknownMarked = true
	return nil
}

func (s *testStore) Release(_ context.Context, claim Claim) error {
	if claim.OwnerToken != s.claim.OwnerToken {
		return errors.New("unexpected claim release")
	}
	s.released = true
	return nil
}

type testResponder struct {
	calls    int
	prompt   string
	history  []domain.Message
	response string
	err      error
}

func (r *testResponder) Respond(_ context.Context, prompt string, history []domain.Message) (string, error) {
	r.calls++
	r.prompt = prompt
	r.history = append([]domain.Message(nil), history...)
	return r.response, r.err
}

type testMessenger struct {
	sent []domain.Outgoing
	err  error
}

func (m *testMessenger) Send(_ context.Context, message domain.Outgoing) (int64, error) {
	m.sent = append(m.sent, message)
	if m.err != nil {
		return 0, m.err
	}
	return int64(len(m.sent)), nil
}
