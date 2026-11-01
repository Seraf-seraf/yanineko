package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Seraf-seraf/yanineko/internal/domain"
	"github.com/Seraf-seraf/yanineko/internal/observability"
)

const AboutMessage = "Я Telegram-бот с ИИ-персонажем. В личных чатах отвечаю на сообщения, а в группах — когда меня упоминают или отвечают на моё сообщение."

const ConversationLeaseDuration = 50 * time.Second
const DeliveryLeaseDuration = 10 * time.Second
const MaxConversationTextBytes = 48 * 1024
const MaxTelegramMessageRunes = domain.MaxOutgoingTextRunes
const MediaResponseTimeout = 22 * time.Second

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
	UpdateUserMessage(context.Context, Claim, string) error
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

type MediaDescriber interface {
	Describe(context.Context, int64, []domain.Image) (string, error)
}

type Messenger interface {
	Send(context.Context, domain.Outgoing) (int64, error)
}

type Service struct {
	SystemPrompt   string
	Store          Store
	Responder      Responder
	MediaDescriber MediaDescriber
	Memory         domain.AgentMemory
	Messenger      Messenger
}

func (s Service) Handle(ctx context.Context, incoming domain.Incoming) error {
	if s.Store == nil || s.Messenger == nil {
		observability.Start(ctx, incoming.UpdateID, "service_dependencies").Fail(ctx, "dependencies_unavailable")
		return errors.New("service dependencies unavailable")
	}
	claimStage := observability.Start(ctx, incoming.UpdateID, "conversation_claim")
	result, err := s.Store.Begin(ctx, incoming, ConversationLeaseDuration)
	if err != nil {
		claimStage.Fail(ctx, "store_begin_failed")
		return errors.Join(errors.New("begin update failed"), err)
	}

	switch result.Status {
	case BeginBusy:
		claimStage.Warn(ctx, "conversation_busy", "status", beginStatusName(result.Status))
		return ErrBusy
	case BeginSent:
		claimStage.Outcome(ctx, "already_processed", "status", beginStatusName(result.Status))
		return nil
	case BeginUnknown:
		claimStage.Warn(ctx, "delivery_state_unknown", "status", beginStatusName(result.Status))
		return nil
	case BeginReady:
		claimStage.Complete(ctx, "status", beginStatusName(result.Status))
		err = s.deliver(ctx, result.Claim.Incoming.UpdateID)
	case BeginClaimed:
		claimStage.Complete(ctx, "status", beginStatusName(result.Status))
		if result.Claim.Incoming.UpdateID == incoming.UpdateID {
			result.Claim.Incoming.Images = incoming.Images
			result.Claim.Incoming.MediaUnavailable = incoming.MediaUnavailable
		}
		err = s.processClaim(ctx, result.Claim)
	default:
		claimStage.Fail(ctx, "invalid_begin_status")
		return errors.New("store returned invalid begin status")
	}
	if err != nil {
		return err
	}
	if result.RetryCurrent {
		observability.Start(ctx, incoming.UpdateID, "conversation_retry").Warn(ctx, "retry_requested")
		return ErrBusy
	}
	return nil
}

func (s Service) processClaim(ctx context.Context, claim Claim) error {
	if claim.Incoming.Kind == domain.MessageAbout {
		aboutStage := observability.Start(ctx, claim.Incoming.UpdateID, "about_response_commit")
		if err := s.Store.CommitAbout(ctx, claim, AboutMessage); err != nil {
			aboutStage.Fail(ctx, "store_about_commit_failed")
			return errors.Join(errors.New("save about response failed"), err)
		}
		aboutStage.Complete(ctx)
		return s.deliver(ctx, claim.Incoming.UpdateID)
	}
	if claim.Incoming.Kind != domain.MessageText {
		observability.Start(ctx, claim.Incoming.UpdateID, "message_validation").Fail(ctx, "unsupported_message_kind")
		return s.abort(ctx, claim, errors.New("unsupported message kind"))
	}
	if s.Responder == nil {
		observability.Start(ctx, claim.Incoming.UpdateID, "response_generation").Fail(ctx, "responder_unavailable")
		return s.abort(ctx, claim, errors.New("responder unavailable"))
	}
	if len(claim.Incoming.Images) > 0 {
		if s.MediaDescriber == nil {
			observability.Start(ctx, claim.Incoming.UpdateID, "media_description").Fail(ctx, "describer_unavailable")
			return s.abort(ctx, claim, errors.New("media describer unavailable"))
		}
		descriptionStage := observability.Start(ctx, claim.Incoming.UpdateID, "media_description")
		description, err := s.MediaDescriber.Describe(ctx, claim.Incoming.UpdateID, claim.Incoming.Images)
		if err != nil {
			descriptionStage.Outcome(ctx, "failed", "code", "media_description_failed")
			return s.abort(ctx, claim, errors.Join(errors.New("describe incoming media failed"), err))
		}
		transcript, err := appendMediaDescription(claim.Incoming.Text, description)
		if err != nil {
			descriptionStage.Fail(ctx, "media_description_too_large")
			return s.abort(ctx, claim, err)
		}
		if err := s.Store.UpdateUserMessage(ctx, claim, transcript); err != nil {
			descriptionStage.Fail(ctx, "archive_media_description_failed")
			return s.abort(ctx, claim, errors.Join(errors.New("save media description failed"), err))
		}
		descriptionStage.Complete(ctx, "image_count", len(claim.Incoming.Images), "description_runes", len([]rune(description)))
	} else if claim.Incoming.MediaUnavailable {
		transcript := strings.TrimSpace(claim.Incoming.Text) + "\n\n[Вложение не удалось загрузить или разобрать.]"
		if err := s.Store.UpdateUserMessage(ctx, claim, transcript); err != nil {
			observability.Start(ctx, claim.Incoming.UpdateID, "media_description").Fail(ctx, "archive_media_error_failed")
			return s.abort(ctx, claim, errors.Join(errors.New("save media error failed"), err))
		}
	}

	modelContextStage := observability.Start(ctx, claim.Incoming.UpdateID, "model_context_load")
	modelContext, err := s.Store.ModelContext(ctx, claim)
	if err != nil {
		modelContextStage.Fail(ctx, "model_context_load_failed")
		return s.abort(ctx, claim, errors.Join(errors.New("load model context failed"), err))
	}
	modelContextStage.Complete(ctx, "initialized", modelContext.Initialized, "through_message_order", modelContext.ThroughMessageOrder)
	historyStage := observability.Start(ctx, claim.Incoming.UpdateID, "conversation_history_load")
	history, err := s.Store.History(ctx, claim, modelContext.ThroughMessageOrder)
	if err != nil {
		historyStage.Fail(ctx, "conversation_history_load_failed")
		return s.abort(ctx, claim, errors.Join(errors.New("load conversation failed"), err))
	}
	historyStage.Complete(ctx, "message_count", len(history))
	responseStage := observability.Start(ctx, claim.Incoming.UpdateID, "response_generation")
	responseContext := ctx
	var cancelResponse context.CancelFunc
	if len(claim.Incoming.Images) > 0 {
		responseContext, cancelResponse = context.WithTimeout(ctx, MediaResponseTimeout)
		defer cancelResponse()
	}
	result, err := s.Responder.Respond(responseContext, domain.AgentRequest{
		UpdateID:     claim.Incoming.UpdateID,
		Instructions: s.SystemPrompt,
		Context:      modelContext,
		Messages:     history,
	}, s.Memory)
	if err != nil {
		responseStage.Outcome(ctx, "failed", "code", "response_generation_failed")
		return s.abort(ctx, claim, errors.Join(errors.New("response generation failed"), err))
	}
	responseStage.Complete(ctx)
	response := strings.TrimSpace(result.Text)
	if response == "" {
		observability.Start(ctx, claim.Incoming.UpdateID, "response_validation").Fail(ctx, "empty_response")
		return s.abort(ctx, claim, errors.New("empty generated response"))
	}
	if len(result.ItemsJSON) == 0 || !json.Valid(result.ItemsJSON) || result.ItemsJSON[0] != '[' {
		observability.Start(ctx, claim.Incoming.UpdateID, "response_validation").Fail(ctx, "invalid_model_context")
		return s.abort(ctx, claim, errors.New("generated model context is invalid"))
	}
	response = limitTelegramText(response)
	modelContext = domain.ModelContext{
		ThroughMessageOrder: modelContext.ThroughMessageOrder,
		ItemsJSON:           result.ItemsJSON,
		Initialized:         true,
	}
	commitStage := observability.Start(ctx, claim.Incoming.UpdateID, "response_commit")
	if err := s.Store.CommitResponse(ctx, claim, response, modelContext); err != nil {
		commitStage.Fail(ctx, "response_commit_failed")
		return errors.Join(errors.New("save generated response failed"), err)
	}
	commitStage.Complete(ctx, "response_runes", len([]rune(response)), "model_context_bytes", len(modelContext.ItemsJSON))
	return s.deliver(ctx, claim.Incoming.UpdateID)
}

func appendMediaDescription(message string, description string) (string, error) {
	description = strings.TrimSpace(description)
	if description == "" || !utf8.ValidString(description) || strings.ContainsRune(description, '\x00') {
		return "", errors.New("invalid media description")
	}
	result := strings.TrimSpace(message) + "\n\n[Описание изображения: " + description + "]"
	if len(result) > MaxConversationTextBytes {
		return "", ErrInputTooLarge
	}
	return result, nil
}

func (s Service) deliver(ctx context.Context, updateID int64) error {
	claimStage := observability.Start(ctx, updateID, "delivery_claim")
	result, err := s.Store.ClaimDelivery(ctx, updateID, DeliveryLeaseDuration)
	if err != nil {
		claimStage.Fail(ctx, "delivery_claim_failed")
		return errors.Join(errors.New("claim telegram delivery failed"), err)
	}
	switch result.Status {
	case DeliveryBusy:
		claimStage.Warn(ctx, "delivery_busy", "status", deliveryStatusName(result.Status))
		return ErrBusy
	case DeliverySent, DeliveryUnknownState:
		if result.Status == DeliverySent {
			claimStage.Outcome(ctx, "already_sent", "status", deliveryStatusName(result.Status))
		} else {
			claimStage.Warn(ctx, "delivery_state_unknown", "status", deliveryStatusName(result.Status))
		}
		return nil
	case DeliveryClaimed:
		claimStage.Complete(ctx, "status", deliveryStatusName(result.Status))
	default:
		claimStage.Fail(ctx, "invalid_delivery_status")
		return errors.New("store returned invalid delivery status")
	}

	sendStage := observability.Start(ctx, updateID, "telegram_send")
	messageID, err := s.Messenger.Send(ctx, result.Delivery.Outgoing)
	if err != nil || messageID <= 0 {
		sendStage.Fail(ctx, "telegram_send_failed")
		unknownStage := observability.Start(ctx, updateID, "delivery_mark_unknown")
		markErr := s.Store.MarkDeliveryUnknown(ctx, result.Delivery)
		if markErr != nil {
			unknownStage.Fail(ctx, "delivery_unknown_mark_failed")
		} else {
			unknownStage.Complete(ctx)
		}
		return errors.Join(ErrDeliveryUnknown, err, markErr)
	}
	sendStage.Complete(ctx)
	markSentStage := observability.Start(ctx, updateID, "delivery_mark_sent")
	if err := s.Store.MarkSent(ctx, result.Delivery, messageID); err != nil {
		markSentStage.Fail(ctx, "delivery_sent_mark_failed")
		return errors.Join(errors.New("save telegram delivery result failed"), err)
	}
	markSentStage.Complete(ctx)
	return nil
}

func beginStatusName(status BeginStatus) string {
	switch status {
	case BeginClaimed:
		return "claimed"
	case BeginReady:
		return "delivery_ready"
	case BeginSent:
		return "already_sent"
	case BeginBusy:
		return "busy"
	case BeginUnknown:
		return "unknown"
	default:
		return "invalid"
	}
}

func deliveryStatusName(status DeliveryStatus) string {
	switch status {
	case DeliveryClaimed:
		return "claimed"
	case DeliveryBusy:
		return "busy"
	case DeliverySent:
		return "sent"
	case DeliveryUnknownState:
		return "unknown"
	default:
		return "invalid"
	}
}

func (s Service) abort(ctx context.Context, claim Claim, cause error) error {
	releaseStage := observability.Start(ctx, claim.Incoming.UpdateID, "conversation_claim_release")
	if err := s.Store.Release(ctx, claim); err != nil {
		releaseStage.Fail(ctx, "claim_release_failed")
		return errors.Join(cause, err)
	}
	releaseStage.Complete(ctx)
	return cause
}

func limitTelegramText(text string) string {
	if utf8.RuneCountInString(text) <= MaxTelegramMessageRunes {
		return text
	}
	return string([]rune(text)[:MaxTelegramMessageRunes])
}
