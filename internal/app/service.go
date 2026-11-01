package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Seraf-seraf/yanineko/internal/domain"
)

const AboutMessage = "Я Telegram-бот с ИИ-персонажем. В личных чатах отвечаю на сообщения, а в группах — когда меня упоминают или отвечают на моё сообщение."

const ConversationLeaseDuration = 50 * time.Second
const DeliveryLeaseDuration = 10 * time.Second
const MaxConversationTextBytes = 48 * 1024
const MaxTelegramMessageRunes = domain.MaxOutgoingTextRunes

var ErrBusy = errors.New("conversation update is already processing")
var ErrDeliveryUnknown = errors.New("telegram delivery status is unknown")
var ErrInputTooLarge = errors.New("conversation input exceeds limit")
var ErrInvalidHistory = errors.New("conversation history is invalid")

type BeginStatus uint8

const (
	BeginClaimed BeginStatus = iota + 1
	BeginReady
	BeginSent
	BeginBusy
	BeginUnknown
)

type Claim struct {
	Incoming   domain.Incoming
	OwnerToken string
	Version    int64
}

type BeginResult struct {
	Status       BeginStatus
	Claim        Claim
	RetryCurrent bool
}

type DeliveryStatus uint8

const (
	DeliveryClaimed DeliveryStatus = iota + 1
	DeliveryBusy
	DeliverySent
	DeliveryUnknownState
)

type Delivery struct {
	UpdateID        int64
	OwnerToken      string
	ProcessingOwner string
	Outgoing        domain.Outgoing
}

type DeliveryResult struct {
	Status   DeliveryStatus
	Delivery Delivery
}

type Store interface {
	Begin(context.Context, domain.Incoming, time.Duration) (BeginResult, error)
	History(context.Context, Claim, int64) ([]domain.Message, error)
	ModelContext(context.Context, Claim) (domain.ModelContext, error)
	CommitResponse(context.Context, Claim, string, domain.ModelContext) error
	CommitAbout(context.Context, Claim, string) error
	ClaimDelivery(context.Context, int64, time.Duration) (DeliveryResult, error)
	MarkSent(context.Context, Delivery, int64) error
	MarkDeliveryUnknown(context.Context, Delivery) error
	Release(context.Context, Claim) error
}

type Responder interface {
	Respond(context.Context, domain.AgentRequest, domain.AgentMemory) (domain.AgentResult, error)
}

type Messenger interface {
	Send(context.Context, domain.Outgoing) (int64, error)
}

type Service struct {
	SystemPrompt string
	Store        Store
	Responder    Responder
	Memory       domain.AgentMemory
	Messenger    Messenger
}

func (s Service) Handle(ctx context.Context, incoming domain.Incoming) error {
	if s.Store == nil || s.Messenger == nil {
		return errors.New("service dependencies unavailable")
	}
	result, err := s.Store.Begin(ctx, incoming, ConversationLeaseDuration)
	if err != nil {
		return errors.Join(errors.New("begin update failed"), err)
	}

	switch result.Status {
	case BeginBusy:
		return ErrBusy
	case BeginSent, BeginUnknown:
		return nil
	case BeginReady:
		err = s.deliver(ctx, result.Claim.Incoming.UpdateID)
	case BeginClaimed:
		err = s.processClaim(ctx, result.Claim)
	default:
		return errors.New("store returned invalid begin status")
	}
	if err != nil {
		return err
	}
	if result.RetryCurrent {
		return ErrBusy
	}
	return nil
}

func (s Service) processClaim(ctx context.Context, claim Claim) error {
	if claim.Incoming.Kind == domain.MessageAbout {
		if err := s.Store.CommitAbout(ctx, claim, AboutMessage); err != nil {
			return errors.Join(errors.New("save about response failed"), err)
		}
		return s.deliver(ctx, claim.Incoming.UpdateID)
	}
	if claim.Incoming.Kind != domain.MessageText {
		return s.abort(ctx, claim, errors.New("unsupported message kind"))
	}
	if s.Responder == nil {
		return s.abort(ctx, claim, errors.New("responder unavailable"))
	}

	modelContext, err := s.Store.ModelContext(ctx, claim)
	if err != nil {
		return s.abort(ctx, claim, errors.Join(errors.New("load model context failed"), err))
	}
	history, err := s.Store.History(ctx, claim, modelContext.ThroughMessageOrder)
	if err != nil {
		return s.abort(ctx, claim, errors.Join(errors.New("load conversation failed"), err))
	}
	result, err := s.Responder.Respond(ctx, domain.AgentRequest{
		Instructions: s.SystemPrompt,
		Context:      modelContext,
		Messages:     history,
	}, s.Memory)
	if err != nil {
		return s.abort(ctx, claim, errors.Join(errors.New("response generation failed"), err))
	}
	response := strings.TrimSpace(result.Text)
	if response == "" {
		return s.abort(ctx, claim, errors.New("empty generated response"))
	}
	if len(result.ItemsJSON) == 0 || !json.Valid(result.ItemsJSON) || result.ItemsJSON[0] != '[' {
		return s.abort(ctx, claim, errors.New("generated model context is invalid"))
	}
	response = limitTelegramText(response)
	modelContext = domain.ModelContext{
		ThroughMessageOrder: modelContext.ThroughMessageOrder,
		ItemsJSON:           result.ItemsJSON,
		Initialized:         true,
	}
	if err := s.Store.CommitResponse(ctx, claim, response, modelContext); err != nil {
		return errors.Join(errors.New("save generated response failed"), err)
	}
	return s.deliver(ctx, claim.Incoming.UpdateID)
}

func (s Service) deliver(ctx context.Context, updateID int64) error {
	result, err := s.Store.ClaimDelivery(ctx, updateID, DeliveryLeaseDuration)
	if err != nil {
		return errors.Join(errors.New("claim telegram delivery failed"), err)
	}
	switch result.Status {
	case DeliveryBusy:
		return ErrBusy
	case DeliverySent, DeliveryUnknownState:
		return nil
	case DeliveryClaimed:
	default:
		return errors.New("store returned invalid delivery status")
	}

	messageID, err := s.Messenger.Send(ctx, result.Delivery.Outgoing)
	if err != nil || messageID <= 0 {
		markErr := s.Store.MarkDeliveryUnknown(ctx, result.Delivery)
		return errors.Join(ErrDeliveryUnknown, err, markErr)
	}
	if err := s.Store.MarkSent(ctx, result.Delivery, messageID); err != nil {
		return errors.Join(errors.New("save telegram delivery result failed"), err)
	}
	return nil
}

func (s Service) abort(ctx context.Context, claim Claim, cause error) error {
	if err := s.Store.Release(ctx, claim); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func limitTelegramText(text string) string {
	if utf8.RuneCountInString(text) <= MaxTelegramMessageRunes {
		return text
	}
	return string([]rune(text)[:MaxTelegramMessageRunes])
}
