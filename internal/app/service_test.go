package app

import (
	"context"
	"encoding/json"
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
	store.history = []domain.Message{
		{UpdateID: 40, ArchiveOrder: 1, Role: domain.RoleUser, Text: "Как дела?"},
		{UpdateID: 40, ArchiveOrder: 2, Role: domain.RoleAssistant, Text: "Хорошо."},
	}
	responder := &testResponder{response: "  Привет!  "}
	messenger := &testMessenger{}
	service := Service{SystemPrompt: "Точный\nмногострочный промпт", Store: store, Responder: responder, Messenger: messenger}

	if err := service.Handle(context.Background(), input); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if responder.request.Instructions != service.SystemPrompt {
		t.Fatalf("prompt = %q, want exact configured prompt", responder.request.Instructions)
	}
	if responder.request.UpdateID != input.UpdateID {
		t.Fatalf("request update ID = %d, want %d", responder.request.UpdateID, input.UpdateID)
	}
	wantHistory := []domain.Message{
		{UpdateID: 40, ArchiveOrder: 1, Role: domain.RoleUser, Text: "Как дела?"},
		{UpdateID: 40, ArchiveOrder: 2, Role: domain.RoleAssistant, Text: "Хорошо."},
		{UpdateID: input.UpdateID, ArchiveOrder: 3, Role: domain.RoleUser, Text: input.Text},
	}
	if !equalMessages(responder.request.Messages, wantHistory) {
		t.Fatalf("history = %#v, want %#v", responder.request.Messages, wantHistory)
	}
	if store.response != "Привет!" {
		t.Fatalf("stored response = %q, want trimmed text", store.response)
	}
	if !store.modelContext.Initialized || store.modelContext.ThroughMessageOrder != 4 {
		t.Fatalf("committed model context = %#v, want initialized checkpoint", store.modelContext)
	}
	if len(messenger.sent) != 1 || messenger.sent[0].Text != "Привет!" {
		t.Fatalf("sent messages = %#v, want one trimmed response", messenger.sent)
	}
	if messenger.sent[0].Key != input.Key || messenger.sent[0].ReplyToMessageID != input.MessageID {
		t.Fatalf("outgoing target = %#v, want input conversation and message", messenger.sent)
	}
}

func TestHandleMediaStoresDescriptionAndPassesTextOnlyToResponder(t *testing.T) {
	t.Parallel()

	input := sampleIncoming(domain.MessageText, "Что на картинке?")
	input.Images = []domain.Image{{Data: []byte("ephemeral png bytes"), MIMEType: "image/png"}}
	store := newTestStore(input)
	describer := &testMediaDescriber{description: "На изображении чёрная кошка рядом с красным мячом."}
	responder := &testResponder{response: "Кошка присматривается к мячу."}
	service := Service{
		SystemPrompt:   "Ты Табакошка.",
		Store:          store,
		Responder:      responder,
		MediaDescriber: describer,
		Messenger:      &testMessenger{},
	}

	if err := service.Handle(context.Background(), input); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	wantUserText := "Что на картинке?\n\n[Описание изображения: На изображении чёрная кошка рядом с красным мячом.]"
	if len(responder.request.Messages) != 1 || responder.request.Messages[0].Text != wantUserText {
		t.Fatalf("responder history = %#v, want only described user text", responder.request.Messages)
	}
	if len(describer.images) != 1 || string(describer.images[0].Data) != "ephemeral png bytes" {
		t.Fatalf("describer images = %#v, want transient image bytes", describer.images)
	}
	if len(responder.request.Context.ItemsJSON) != 2 || string(responder.request.Context.ItemsJSON) != "[]" {
		t.Fatalf("responder model context = %s, want empty prior context", responder.request.Context.ItemsJSON)
	}
	if len(store.history) < 1 || store.history[0].Text != wantUserText {
		t.Fatalf("archived history = %#v, want media description in user transcript", store.history)
	}
	if strings.Contains(string(store.committed.ItemsJSON), "ephemeral png bytes") {
		t.Fatal("persisted model context contains media bytes")
	}
}

func TestHandleMediaUnavailableArchivesNoticeWithoutChangingInputText(t *testing.T) {
	t.Parallel()

	input := sampleIncoming(domain.MessageText, "Что на фото?")
	input.MediaUnavailable = true
	store := newTestStore(input)
	responder := &testResponder{response: "Не получилось рассмотреть вложение."}
	service := Service{Store: store, Responder: responder, Messenger: &testMessenger{}}

	if err := service.Handle(context.Background(), input); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	wantTranscript := "Что на фото?\n\n[Вложение не удалось загрузить или разобрать.]"
	if len(responder.request.Messages) != 1 || responder.request.Messages[0].Text != wantTranscript {
		t.Fatalf("responder history = %#v, want media failure notice", responder.request.Messages)
	}
	if len(store.history) != 1 || store.history[0].Text != wantTranscript {
		t.Fatalf("archived history = %#v, want media failure notice", store.history)
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
	if len(store.history) != 1 || store.history[0].Role != domain.RoleUser || store.history[0].Text != input.Text {
		t.Fatalf("archive after generation failure = %#v, want failed input retained", store.history)
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

func TestHandlePassesEntireUncompactedHistory(t *testing.T) {
	t.Parallel()

	input := sampleIncoming(domain.MessageText, "current")
	store := newTestStore(input)
	previous := make([]domain.Message, 0, 60)
	for index := range 30 {
		previous = append(previous,
			domain.Message{UpdateID: int64(index + 1), ArchiveOrder: int64(index*2 + 1), Role: domain.RoleUser, Text: "u" + string(rune('a'+index))},
			domain.Message{UpdateID: int64(index + 1), ArchiveOrder: int64(index*2 + 2), Role: domain.RoleAssistant, Text: "a" + string(rune('a'+index))})
	}
	store.history = previous
	responder := &testResponder{response: "reply"}
	service := Service{Store: store, Responder: responder, Messenger: &testMessenger{}}
	if err := service.Handle(context.Background(), input); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if got, want := len(responder.request.Messages), len(previous)+1; got != want {
		t.Fatalf("history length = %d, want all %d messages", got, want)
	}
	if responder.request.Messages[0].Text != previous[0].Text || responder.request.Messages[len(responder.request.Messages)-1].Text != input.Text {
		t.Fatalf("history boundaries = %#v, want oldest archived message through current input", responder.request.Messages)
	}
}

func TestHandlePassesOnlyMessagesAfterModelCheckpoint(t *testing.T) {
	t.Parallel()

	input := sampleIncoming(domain.MessageText, "new message")
	store := newTestStore(input)
	store.modelContext = domain.ModelContext{ThroughMessageOrder: 90, Initialized: true, ItemsJSON: json.RawMessage(`[{"type":"message","role":"assistant"}]`)}
	store.history = []domain.Message{
		{UpdateID: 80, ArchiveOrder: 80, Role: domain.RoleUser, Text: "already in context"},
		{UpdateID: 80, ArchiveOrder: 90, Role: domain.RoleAssistant, Text: "already answered"},
	}
	responder := &testResponder{response: "reply"}
	service := Service{Store: store, Responder: responder, Messenger: &testMessenger{}}
	if err := service.Handle(context.Background(), input); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if len(responder.request.Messages) != 1 || responder.request.Messages[0].Text != input.Text {
		t.Fatalf("incremental messages = %#v, want only current message", responder.request.Messages)
	}
	if responder.request.Context.ThroughMessageOrder != 90 {
		t.Fatalf("context watermark = %d, want 90", responder.request.Context.ThroughMessageOrder)
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
	modelContext  domain.ModelContext
	response      string
	committed     domain.ModelContext
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
		modelContext: domain.ModelContext{
			ThroughMessageOrder: 0,
			ItemsJSON:           json.RawMessage("[]"),
		},
		delivery: Delivery{
			UpdateID: incoming.UpdateID, OwnerToken: strings.Repeat("b", 32),
			ProcessingOwner: claim.OwnerToken,
			Outgoing:        domain.Outgoing{Key: incoming.Key, ReplyToMessageID: incoming.MessageID},
		},
	}
}

func (s *testStore) Begin(_ context.Context, incoming domain.Incoming, duration time.Duration) (BeginResult, error) {
	if duration != ConversationLeaseDuration {
		return BeginResult{}, errors.New("unexpected conversation lease")
	}
	if s.begin.Status == BeginClaimed && incoming.Kind == domain.MessageText {
		found := false
		for _, message := range s.history {
			if message.UpdateID == incoming.UpdateID && message.Role == domain.RoleUser {
				found = true
				break
			}
		}
		if !found {
			archiveOrder := int64(1)
			for _, message := range s.history {
				if message.ArchiveOrder >= archiveOrder {
					archiveOrder = message.ArchiveOrder + 1
				}
			}
			s.history = append(s.history, domain.Message{UpdateID: incoming.UpdateID, ArchiveOrder: archiveOrder, Role: domain.RoleUser, Text: incoming.Text})
		}
	}
	return s.begin, nil
}

func (s *testStore) History(_ context.Context, claim Claim, afterMessageOrder int64) ([]domain.Message, error) {
	s.claim = claim
	var history []domain.Message
	for _, message := range s.history {
		if message.ArchiveOrder > afterMessageOrder {
			history = append(history, message)
		}
	}
	return history, nil
}

func (s *testStore) UpdateUserMessage(_ context.Context, claim Claim, content string) error {
	for index := range s.history {
		message := &s.history[index]
		if message.UpdateID == claim.Incoming.UpdateID && message.Role == domain.RoleUser {
			message.Text = content
			return nil
		}
	}
	return errors.New("archived user message not found")
}

func (s *testStore) ModelContext(_ context.Context, claim Claim) (domain.ModelContext, error) {
	s.claim = claim
	return s.modelContext, nil
}

func (s *testStore) CommitResponse(_ context.Context, claim Claim, response string, modelContext domain.ModelContext) error {
	var maxOrder int64
	for _, message := range s.history {
		if message.ArchiveOrder > maxOrder {
			maxOrder = message.ArchiveOrder
		}
	}
	modelContext.ThroughMessageOrder = maxOrder + 1
	s.claim, s.response, s.modelContext, s.committed = claim, response, modelContext, modelContext
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
	request  domain.AgentRequest
	response string
	err      error
}

type testMediaDescriber struct {
	description string
	images      []domain.Image
	err         error
}

func (d *testMediaDescriber) Describe(_ context.Context, _ int64, images []domain.Image) (string, error) {
	d.images = images
	return d.description, d.err
}

func (r *testResponder) Respond(_ context.Context, request domain.AgentRequest, _ domain.AgentMemory) (domain.AgentResult, error) {
	r.calls++
	r.request = request
	if r.err != nil {
		return domain.AgentResult{}, r.err
	}
	return domain.AgentResult{Text: r.response, ItemsJSON: json.RawMessage("[]")}, nil
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
