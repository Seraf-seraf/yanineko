package postgres

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Seraf-seraf/yanineko/internal/app"
	"github.com/Seraf-seraf/yanineko/internal/domain"
)

const cleanupBatchSize = 100
const earlierPendingScanLimit = 100
const processedUpdateRetention = 90 * 24 * time.Hour
const migrationLockKey int64 = 6501110212279086159

var ErrDatabaseConfiguration = errors.New("invalid PostgreSQL configuration")
var ErrDatabaseUnavailable = errors.New("PostgreSQL unavailable")
var ErrDatabaseOperation = errors.New("PostgreSQL operation failed")
var ErrSchemaMigration = errors.New("PostgreSQL schema migration failed")
var ErrLeaseLost = errors.New("conversation lease lost")
var ErrUpdateConflict = errors.New("Telegram update id conflict")
var ErrUpdateNotFound = errors.New("Telegram update not found")

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct {
	pool *pgxpool.Pool
}

type conversationRecord struct {
	Version    int64
	LeaseOwner pgtype.Text
	LeaseUntil pgtype.Timestamptz
	Now        time.Time
}

type updateRecord struct {
	UpdateID           int64
	ChatID             int64
	ThreadID           int64
	SourceMessageID    int64
	InputText          string
	Kind               string
	State              string
	ProcessingOwner    string
	ProcessingVersion  int64
	ResponseText       pgtype.Text
	TelegramMessageID  pgtype.Int8
	DeliveryOwner      pgtype.Text
	DeliveryLeaseUntil pgtype.Timestamptz
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	if !validDatabaseURL(databaseURL) {
		return nil, ErrDatabaseConfiguration
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, ErrDatabaseConfiguration
	}
	poolConfig.MaxConns = 1
	poolConfig.MinConns = 0
	poolConfig.MaxConnLifetime = time.Minute
	poolConfig.MaxConnIdleTime = 10 * time.Second
	poolConfig.PingTimeout = 4 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, databaseError(ctx, err, ErrDatabaseUnavailable)
	}
	store := newStore(pool)
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, databaseError(ctx, err, ErrDatabaseUnavailable)
	}
	if err := store.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return store, nil
}

func newStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return databaseError(ctx, err, ErrSchemaMigration)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockKey); err != nil {
		return databaseError(ctx, err, ErrSchemaMigration)
	}
	if _, err := tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS yanineko_schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
		)`); err != nil {
		return databaseError(ctx, err, ErrSchemaMigration)
	}
	for version, name := range []string{"migrations/001_initial.sql", "migrations/002_model_contexts.sql", "migrations/003_archive_order.sql"} {
		migrationVersion := version + 1
		var applied bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM yanineko_schema_migrations WHERE version = $1)`, migrationVersion).Scan(&applied); err != nil {
			return databaseError(ctx, err, ErrSchemaMigration)
		}
		if applied {
			continue
		}
		script, err := migrations.ReadFile(name)
		if err != nil {
			return ErrSchemaMigration
		}
		if _, err := tx.Conn().PgConn().Exec(ctx, string(script)).ReadAll(); err != nil {
			return databaseError(ctx, err, ErrSchemaMigration)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO yanineko_schema_migrations (version) VALUES ($1)`, migrationVersion); err != nil {
			return databaseError(ctx, err, ErrSchemaMigration)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return databaseError(ctx, err, ErrSchemaMigration)
	}
	return nil
}

func (s *Store) Begin(ctx context.Context, incoming domain.Incoming, leaseDuration time.Duration) (app.BeginResult, error) {
	if !validIncoming(incoming) || !validateDuration(leaseDuration) {
		return app.BeginResult{}, ErrDatabaseConfiguration
	}
	if err := s.cleanupStale(ctx); err != nil {
		return app.BeginResult{}, err
	}
	existingKey, exists, err := s.findUpdateKey(ctx, incoming.UpdateID)
	if err != nil {
		return app.BeginResult{}, err
	}
	if exists && existingKey != incoming.Key {
		return app.BeginResult{}, ErrUpdateConflict
	}
	owner, err := ownerToken()
	if err != nil {
		return app.BeginResult{}, ErrDatabaseOperation
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return app.BeginResult{}, databaseError(ctx, err, ErrDatabaseOperation)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO conversations (chat_id, thread_id)
		VALUES ($1, $2)
		ON CONFLICT (chat_id, thread_id) DO NOTHING`, incoming.Key.ChatID, incoming.Key.ThreadID); err != nil {
		return app.BeginResult{}, databaseError(ctx, err, ErrDatabaseOperation)
	}
	conversation, err := lockConversation(ctx, tx, incoming.Key)
	if err != nil {
		return app.BeginResult{}, err
	}
	existing, err := lockUpdate(ctx, tx, incoming.UpdateID)
	if err == nil {
		if !matchesIncoming(existing, incoming) {
			return app.BeginResult{}, ErrUpdateConflict
		}
		result, err := s.beginExisting(ctx, tx, conversation, existing, owner, leaseDuration)
		if err != nil {
			return app.BeginResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return app.BeginResult{}, databaseError(ctx, err, ErrDatabaseOperation)
		}
		return result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return app.BeginResult{}, databaseError(ctx, err, ErrDatabaseOperation)
	}

	for scanned := 0; scanned < earlierPendingScanLimit; scanned++ {
		pending, pendingErr := lockPending(ctx, tx, incoming.Key, incoming.UpdateID)
		if pendingErr != nil && !errors.Is(pendingErr, pgx.ErrNoRows) {
			return app.BeginResult{}, databaseError(ctx, pendingErr, ErrDatabaseOperation)
		}
		if errors.Is(pendingErr, pgx.ErrNoRows) {
			break
		}
		result, done, pendingErr := s.beginEarlierPending(ctx, tx, &conversation, pending, owner, leaseDuration)
		if pendingErr != nil {
			return app.BeginResult{}, pendingErr
		}
		if done {
			if err := tx.Commit(ctx); err != nil {
				return app.BeginResult{}, databaseError(ctx, err, ErrDatabaseOperation)
			}
			return result, nil
		}
	}
	remaining, pendingErr := lockPending(ctx, tx, incoming.Key, incoming.UpdateID)
	if pendingErr != nil && !errors.Is(pendingErr, pgx.ErrNoRows) {
		return app.BeginResult{}, databaseError(ctx, pendingErr, ErrDatabaseOperation)
	}
	if pendingErr == nil {
		if err := tx.Commit(ctx); err != nil {
			return app.BeginResult{}, databaseError(ctx, err, ErrDatabaseOperation)
		}
		return app.BeginResult{Status: app.BeginBusy, Claim: claimFromRecord(remaining)}, nil
	}

	if leaseActive(conversation) {
		return app.BeginResult{Status: app.BeginBusy}, nil
	}
	version := conversation.Version + 1
	if err := setConversationLease(ctx, tx, incoming.Key, version, owner, leaseDuration); err != nil {
		return app.BeginResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO processed_updates (
			update_id, chat_id, thread_id, source_message_id, input_text, kind, state,
			processing_owner, processing_version
		) VALUES ($1, $2, $3, $4, $5, $6, 'processing', $7, $8)`,
		incoming.UpdateID, incoming.Key.ChatID, incoming.Key.ThreadID, incoming.MessageID,
		incoming.Text, kindName(incoming.Kind), owner, version); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return app.BeginResult{}, ErrUpdateConflict
		}
		return app.BeginResult{}, databaseError(ctx, err, ErrDatabaseOperation)
	}
	if incoming.Kind == domain.MessageText {
		if err := insertArchivedUserMessage(ctx, tx, incoming.Key, incoming.UpdateID, incoming.Text); err != nil {
			return app.BeginResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return app.BeginResult{}, databaseError(ctx, err, ErrDatabaseOperation)
	}
	return app.BeginResult{Status: app.BeginClaimed, Claim: claimFromIncoming(incoming, owner, version)}, nil
}

func (s *Store) History(ctx context.Context, claim app.Claim, afterUpdateID int64) ([]domain.Message, error) {
	if !validClaim(claim) || claim.Incoming.Kind != domain.MessageText {
		return nil, ErrLeaseLost
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, databaseError(ctx, err, ErrDatabaseOperation)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	conversation, err := lockConversation(ctx, tx, claim.Incoming.Key)
	if err != nil {
		return nil, err
	}
	update, err := lockUpdate(ctx, tx, claim.Incoming.UpdateID)
	if err != nil {
		return nil, safeQueryError(ctx, err)
	}
	if !claimOwns(conversation, update, claim) {
		return nil, ErrLeaseLost
	}
	rows, err := tx.Query(ctx, `
		SELECT update_id, archive_order, role, content
		FROM conversation_messages
		WHERE chat_id = $1 AND thread_id = $2 AND archive_order > $3
		ORDER BY archive_order`,
		claim.Incoming.Key.ChatID, claim.Incoming.Key.ThreadID, afterUpdateID)
	if err != nil {
		return nil, databaseError(ctx, err, ErrDatabaseOperation)
	}
	defer rows.Close()
	history := make([]domain.Message, 0)
	for rows.Next() {
		var message domain.Message
		if err := rows.Scan(&message.UpdateID, &message.ArchiveOrder, &message.Role, &message.Text); err != nil {
			return nil, databaseError(ctx, err, ErrDatabaseOperation)
		}
		history = append(history, message)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError(ctx, err, ErrDatabaseOperation)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, databaseError(ctx, err, ErrDatabaseOperation)
	}
	return history, nil
}

func (s *Store) UpdateUserMessage(ctx context.Context, claim app.Claim, content string) error {
	if !validClaim(claim) || claim.Incoming.Kind != domain.MessageText || strings.TrimSpace(content) == "" ||
		len(content) > app.MaxConversationTextBytes || !utf8.ValidString(content) || strings.ContainsRune(content, '\x00') {
		return ErrDatabaseConfiguration
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	conversation, err := lockConversation(ctx, tx, claim.Incoming.Key)
	if err != nil {
		return err
	}
	update, err := lockUpdate(ctx, tx, claim.Incoming.UpdateID)
	if err != nil {
		return safeQueryError(ctx, err)
	}
	if !claimOwns(conversation, update, claim) {
		return ErrLeaseLost
	}
	result, err := tx.Exec(ctx, `
		UPDATE conversation_messages SET content = $1
		WHERE chat_id = $2 AND thread_id = $3 AND update_id = $4 AND role = 'user'`,
		content, claim.Incoming.Key.ChatID, claim.Incoming.Key.ThreadID, claim.Incoming.UpdateID)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	if err := tx.Commit(ctx); err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	return nil
}

func (s *Store) ModelContext(ctx context.Context, claim app.Claim) (domain.ModelContext, error) {
	if !validClaim(claim) || claim.Incoming.Kind != domain.MessageText {
		return domain.ModelContext{}, ErrLeaseLost
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.ModelContext{}, databaseError(ctx, err, ErrDatabaseOperation)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	conversation, err := lockConversation(ctx, tx, claim.Incoming.Key)
	if err != nil {
		return domain.ModelContext{}, err
	}
	update, err := lockUpdate(ctx, tx, claim.Incoming.UpdateID)
	if err != nil {
		return domain.ModelContext{}, safeQueryError(ctx, err)
	}
	if !claimOwns(conversation, update, claim) {
		return domain.ModelContext{}, ErrLeaseLost
	}
	var modelContext domain.ModelContext
	err = tx.QueryRow(ctx, `
		SELECT through_message_order, items_json
		FROM model_contexts WHERE chat_id = $1 AND thread_id = $2`,
		claim.Incoming.Key.ChatID, claim.Incoming.Key.ThreadID).Scan(
		&modelContext.ThroughMessageOrder, &modelContext.ItemsJSON)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return domain.ModelContext{}, databaseError(ctx, err, ErrDatabaseOperation)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		modelContext.ThroughMessageOrder = 0
		modelContext.ItemsJSON = json.RawMessage("[]")
		modelContext.Initialized = false
	} else {
		modelContext.Initialized = true
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ModelContext{}, databaseError(ctx, err, ErrDatabaseOperation)
	}
	return modelContext, nil
}

func (s *Store) CommitResponse(ctx context.Context, claim app.Claim, response string, modelContext domain.ModelContext) error {
	if claim.Incoming.Kind != domain.MessageText {
		return ErrLeaseLost
	}
	return s.commitResponse(ctx, claim, response, modelContext)
}

func (s *Store) CommitAbout(ctx context.Context, claim app.Claim, response string) error {
	if claim.Incoming.Kind != domain.MessageAbout {
		return ErrLeaseLost
	}
	return s.commitResponse(ctx, claim, response, domain.ModelContext{})
}

func (s *Store) commitResponse(ctx context.Context, claim app.Claim, response string, modelContext domain.ModelContext) error {
	if !validClaim(claim) || strings.TrimSpace(response) == "" || len(response) > app.MaxConversationTextBytes || !utf8.ValidString(response) || strings.ContainsRune(response, '\x00') {
		return ErrDatabaseConfiguration
	}
	if claim.Incoming.Kind == domain.MessageText && (!modelContext.Initialized || len(modelContext.ItemsJSON) == 0 || !json.Valid(modelContext.ItemsJSON) || modelContext.ItemsJSON[0] != '[') {
		return ErrDatabaseConfiguration
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	conversation, err := lockConversation(ctx, tx, claim.Incoming.Key)
	if err != nil {
		return err
	}
	update, err := lockUpdate(ctx, tx, claim.Incoming.UpdateID)
	if err != nil {
		return safeQueryError(ctx, err)
	}
	if !claimOwns(conversation, update, claim) {
		return ErrLeaseLost
	}
	if claim.Incoming.Kind == domain.MessageText {
		if _, err := tx.Exec(ctx, `
			INSERT INTO conversation_messages (chat_id, thread_id, update_id, archive_order, role, content)
			VALUES ($1, $2, $3, (SELECT COALESCE(MAX(archive_order), 0) + 1 FROM conversation_messages WHERE chat_id = $1 AND thread_id = $2), 'assistant', $4)`,
			claim.Incoming.Key.ChatID, claim.Incoming.Key.ThreadID, claim.Incoming.UpdateID, response); err != nil {
			return databaseError(ctx, err, ErrDatabaseOperation)
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(archive_order), 0) FROM conversation_messages WHERE chat_id = $1 AND thread_id = $2`,
			claim.Incoming.Key.ChatID, claim.Incoming.Key.ThreadID).Scan(&modelContext.ThroughMessageOrder); err != nil {
			return databaseError(ctx, err, ErrDatabaseOperation)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO model_contexts (chat_id, thread_id, through_message_order, items_json)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (chat_id, thread_id) DO UPDATE SET
				through_message_order = EXCLUDED.through_message_order,
				items_json = EXCLUDED.items_json,
				updated_at = clock_timestamp()`,
			claim.Incoming.Key.ChatID, claim.Incoming.Key.ThreadID,
			modelContext.ThroughMessageOrder, string(modelContext.ItemsJSON)); err != nil {
			return databaseError(ctx, err, ErrDatabaseOperation)
		}
	}
	result, err := tx.Exec(ctx, `
		UPDATE processed_updates
		SET state = 'ready', response_text = $2, updated_at = clock_timestamp()
		WHERE update_id = $1 AND state = 'processing'
		  AND processing_owner = $3 AND processing_version = $4`,
		claim.Incoming.UpdateID, response, claim.OwnerToken, claim.Version)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	if err := tx.Commit(ctx); err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	return nil
}

func (s *Store) ClaimDelivery(ctx context.Context, updateID int64, leaseDuration time.Duration) (app.DeliveryResult, error) {
	if updateID < 0 || !validateDuration(leaseDuration) {
		return app.DeliveryResult{}, ErrDatabaseConfiguration
	}
	key, exists, err := s.findUpdateKey(ctx, updateID)
	if err != nil {
		return app.DeliveryResult{}, err
	}
	if !exists {
		return app.DeliveryResult{}, ErrUpdateNotFound
	}
	owner, err := ownerToken()
	if err != nil {
		return app.DeliveryResult{}, ErrDatabaseOperation
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return app.DeliveryResult{}, databaseError(ctx, err, ErrDatabaseOperation)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	conversation, err := lockConversation(ctx, tx, key)
	if err != nil {
		return app.DeliveryResult{}, err
	}
	update, err := lockUpdate(ctx, tx, updateID)
	if err != nil {
		return app.DeliveryResult{}, safeQueryError(ctx, err)
	}
	if (domain.ConversationKey{ChatID: update.ChatID, ThreadID: update.ThreadID}) != key {
		return app.DeliveryResult{}, ErrUpdateConflict
	}
	result := app.DeliveryResult{}
	switch update.State {
	case "ready":
		if !update.ResponseText.Valid || strings.TrimSpace(update.ResponseText.String) == "" {
			return app.DeliveryResult{}, ErrDatabaseOperation
		}
		command, err := tx.Exec(ctx, `
			UPDATE processed_updates
			SET state = 'sending', delivery_owner = $2,
			    delivery_lease_until = clock_timestamp() + ($3::bigint * interval '1 microsecond'),
			    updated_at = clock_timestamp()
			WHERE update_id = $1 AND state = 'ready'`, updateID, owner, leaseDuration.Microseconds())
		if err != nil {
			return app.DeliveryResult{}, databaseError(ctx, err, ErrDatabaseOperation)
		}
		if command.RowsAffected() != 1 {
			return app.DeliveryResult{}, ErrLeaseLost
		}
		result = app.DeliveryResult{
			Status: app.DeliveryClaimed,
			Delivery: app.Delivery{
				UpdateID: update.UpdateID, OwnerToken: owner, ProcessingOwner: update.ProcessingOwner,
				Outgoing: domain.Outgoing{
					Key:              domain.ConversationKey{ChatID: update.ChatID, ThreadID: update.ThreadID},
					ReplyToMessageID: update.SourceMessageID, Text: update.ResponseText.String,
				},
			},
		}
	case "sending":
		if update.DeliveryLeaseUntil.Valid && update.DeliveryLeaseUntil.Time.After(conversation.Now) {
			result.Status = app.DeliveryBusy
		} else {
			if err := markUnknownTx(ctx, tx, update); err != nil {
				return app.DeliveryResult{}, err
			}
			if err := releaseConversation(ctx, tx, key, update.ProcessingOwner, update.ProcessingVersion); err != nil && !errors.Is(err, ErrLeaseLost) {
				return app.DeliveryResult{}, err
			}
			result.Status = app.DeliveryUnknownState
		}
	case "sent":
		result.Status = app.DeliverySent
	case "delivery_unknown":
		result.Status = app.DeliveryUnknownState
	default:
		result.Status = app.DeliveryBusy
	}
	if err := tx.Commit(ctx); err != nil {
		return app.DeliveryResult{}, databaseError(ctx, err, ErrDatabaseOperation)
	}
	return result, nil
}

func (s *Store) MarkSent(ctx context.Context, delivery app.Delivery, telegramMessageID int64) error {
	if !validDelivery(delivery) || telegramMessageID <= 0 {
		return ErrDatabaseConfiguration
	}
	return s.finishDelivery(ctx, delivery, "sent", telegramMessageID)
}

func (s *Store) MarkDeliveryUnknown(ctx context.Context, delivery app.Delivery) error {
	if !validDelivery(delivery) {
		return ErrDatabaseConfiguration
	}
	return s.finishDelivery(ctx, delivery, "delivery_unknown", 0)
}

func (s *Store) finishDelivery(ctx context.Context, delivery app.Delivery, state string, telegramMessageID int64) error {
	key, exists, err := s.findUpdateKey(ctx, delivery.UpdateID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrUpdateNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := lockConversation(ctx, tx, key); err != nil {
		return err
	}
	update, err := lockUpdate(ctx, tx, delivery.UpdateID)
	if err != nil {
		return safeQueryError(ctx, err)
	}
	if !matchesDelivery(update, delivery) {
		return ErrLeaseLost
	}
	var command pgconn.CommandTag
	if state == "sent" {
		command, err = tx.Exec(ctx, `
			UPDATE processed_updates
			SET state = 'sent', telegram_message_id = $2, delivery_owner = NULL,
			    delivery_lease_until = NULL, updated_at = clock_timestamp()
			WHERE update_id = $1 AND state = 'sending' AND delivery_owner = $3`,
			delivery.UpdateID, telegramMessageID, delivery.OwnerToken)
	} else {
		command, err = tx.Exec(ctx, `
			UPDATE processed_updates
			SET state = 'delivery_unknown', delivery_owner = NULL,
			    delivery_lease_until = NULL, updated_at = clock_timestamp()
			WHERE update_id = $1 AND state = 'sending' AND delivery_owner = $2`,
			delivery.UpdateID, delivery.OwnerToken)
	}
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	if command.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	if _, err := tx.Exec(ctx, `
		UPDATE conversations
		SET last_update_id = GREATEST(last_update_id, $3), updated_at = clock_timestamp()
		WHERE chat_id = $1 AND thread_id = $2`, key.ChatID, key.ThreadID, delivery.UpdateID); err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	if err := releaseConversation(ctx, tx, key, delivery.ProcessingOwner, update.ProcessingVersion); err != nil && !errors.Is(err, ErrLeaseLost) {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	return nil
}

func (s *Store) Release(ctx context.Context, claim app.Claim) error {
	if !validClaim(claim) {
		return ErrLeaseLost
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	conversation, err := lockConversation(ctx, tx, claim.Incoming.Key)
	if err != nil {
		return err
	}
	update, err := lockUpdate(ctx, tx, claim.Incoming.UpdateID)
	if err != nil {
		return safeQueryError(ctx, err)
	}
	if !claimOwns(conversation, update, claim) {
		return ErrLeaseLost
	}
	command, err := tx.Exec(ctx, `
		DELETE FROM processed_updates
		WHERE update_id = $1 AND state = 'processing'
		  AND processing_owner = $2 AND processing_version = $3`,
		claim.Incoming.UpdateID, claim.OwnerToken, claim.Version)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	if command.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	if err := releaseConversation(ctx, tx, claim.Incoming.Key, claim.OwnerToken, claim.Version); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	return nil
}

func (s *Store) findUpdateKey(ctx context.Context, updateID int64) (domain.ConversationKey, bool, error) {
	var key domain.ConversationKey
	err := s.pool.QueryRow(ctx, `
		SELECT chat_id, thread_id FROM processed_updates WHERE update_id = $1`, updateID).
		Scan(&key.ChatID, &key.ThreadID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ConversationKey{}, false, nil
	}
	if err != nil {
		return domain.ConversationKey{}, false, databaseError(ctx, err, ErrDatabaseOperation)
	}
	return key, true, nil
}

func validDatabaseURL(databaseURL string) bool {
	parsed, err := url.Parse(databaseURL)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Hostname() == "" || parsed.User == nil || parsed.User.Username() == "" || parsed.Path == "" || parsed.Path == "/" || parsed.Fragment != "" || parsed.Opaque != "" {
		return false
	}
	if _, hasPassword := parsed.User.Password(); !hasPassword {
		return false
	}
	if strings.EqualFold(parsed.Hostname(), "localhost") {
		return true
	}
	if address := net.ParseIP(parsed.Hostname()); address != nil && address.IsLoopback() {
		return true
	}
	switch parsed.Query().Get("sslmode") {
	case "require", "verify-ca", "verify-full":
		return true
	default:
		return false
	}
}

func validClaim(claim app.Claim) bool {
	if !validIncoming(claim.Incoming) || claim.Version <= 0 || len(claim.OwnerToken) != 32 {
		return false
	}
	_, err := hex.DecodeString(claim.OwnerToken)
	return err == nil
}

func claimOwns(conversation conversationRecord, update updateRecord, claim app.Claim) bool {
	return leaseActive(conversation) && conversation.Version == claim.Version &&
		conversation.LeaseOwner.Valid && conversation.LeaseOwner.String == claim.OwnerToken &&
		update.State == "processing" && update.ProcessingOwner == claim.OwnerToken &&
		update.ProcessingVersion == claim.Version && matchesIncoming(update, claim.Incoming)
}

func validDelivery(delivery app.Delivery) bool {
	return delivery.UpdateID >= 0 && len(delivery.OwnerToken) == 32 && len(delivery.ProcessingOwner) == 32 &&
		delivery.Outgoing.Key.ChatID != 0 && delivery.Outgoing.Key.ThreadID >= 0 &&
		delivery.Outgoing.ReplyToMessageID > 0 && strings.TrimSpace(delivery.Outgoing.Text) != "" &&
		utf8.ValidString(delivery.Outgoing.Text) && !strings.ContainsRune(delivery.Outgoing.Text, '\x00')
}

func matchesDelivery(update updateRecord, delivery app.Delivery) bool {
	return update.State == "sending" && update.DeliveryOwner.Valid && update.DeliveryOwner.String == delivery.OwnerToken &&
		update.ProcessingOwner == delivery.ProcessingOwner && update.ChatID == delivery.Outgoing.Key.ChatID &&
		update.ThreadID == delivery.Outgoing.Key.ThreadID && update.SourceMessageID == delivery.Outgoing.ReplyToMessageID &&
		update.ResponseText.Valid && update.ResponseText.String == delivery.Outgoing.Text
}

func (s *Store) beginExisting(ctx context.Context, tx pgx.Tx, conversation conversationRecord, update updateRecord, owner string, leaseDuration time.Duration) (app.BeginResult, error) {
	claim := claimFromRecord(update)
	switch update.State {
	case "processing":
		if update.Kind == "text" {
			if err := insertArchivedUserMessage(ctx, tx, domain.ConversationKey{ChatID: update.ChatID, ThreadID: update.ThreadID}, update.UpdateID, update.InputText); err != nil {
				return app.BeginResult{}, err
			}
		}
		if leaseActive(conversation) {
			return app.BeginResult{Status: app.BeginBusy, Claim: claim}, nil
		}
		version := conversation.Version + 1
		if err := setConversationLease(ctx, tx, domain.ConversationKey{ChatID: update.ChatID, ThreadID: update.ThreadID}, version, owner, leaseDuration); err != nil {
			return app.BeginResult{}, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE processed_updates
			SET processing_owner = $2, processing_version = $3, updated_at = clock_timestamp()
			WHERE update_id = $1 AND state = 'processing'`, update.UpdateID, owner, version); err != nil {
			return app.BeginResult{}, databaseError(ctx, err, ErrDatabaseOperation)
		}
		claim.OwnerToken, claim.Version = owner, version
		return app.BeginResult{Status: app.BeginClaimed, Claim: claim}, nil
	case "ready":
		return app.BeginResult{Status: app.BeginReady, Claim: claim}, nil
	case "sending":
		if update.DeliveryLeaseUntil.Valid && update.DeliveryLeaseUntil.Time.After(conversation.Now) {
			return app.BeginResult{Status: app.BeginBusy, Claim: claim}, nil
		}
		if err := markUnknownTx(ctx, tx, update); err != nil {
			return app.BeginResult{}, err
		}
		if err := releaseConversation(ctx, tx, domain.ConversationKey{ChatID: update.ChatID, ThreadID: update.ThreadID}, update.ProcessingOwner, update.ProcessingVersion); err != nil && !errors.Is(err, ErrLeaseLost) {
			return app.BeginResult{}, err
		}
		return app.BeginResult{Status: app.BeginUnknown, Claim: claim}, nil
	case "sent":
		return app.BeginResult{Status: app.BeginSent, Claim: claim}, nil
	case "delivery_unknown":
		return app.BeginResult{Status: app.BeginUnknown, Claim: claim}, nil
	default:
		return app.BeginResult{}, ErrDatabaseOperation
	}
}

func (s *Store) beginEarlierPending(ctx context.Context, tx pgx.Tx, conversation *conversationRecord, update updateRecord, owner string, leaseDuration time.Duration) (app.BeginResult, bool, error) {
	claim := claimFromRecord(update)
	switch update.State {
	case "processing":
		if update.Kind == "text" {
			if err := insertArchivedUserMessage(ctx, tx, domain.ConversationKey{ChatID: update.ChatID, ThreadID: update.ThreadID}, update.UpdateID, update.InputText); err != nil {
				return app.BeginResult{}, false, err
			}
		}
		if leaseActive(*conversation) {
			return app.BeginResult{Status: app.BeginBusy, Claim: claim}, true, nil
		}
		version := conversation.Version + 1
		if err := setConversationLease(ctx, tx, domain.ConversationKey{ChatID: update.ChatID, ThreadID: update.ThreadID}, version, owner, leaseDuration); err != nil {
			return app.BeginResult{}, false, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE processed_updates
			SET processing_owner = $2, processing_version = $3, updated_at = clock_timestamp()
			WHERE update_id = $1 AND state = 'processing'`, update.UpdateID, owner, version); err != nil {
			return app.BeginResult{}, false, databaseError(ctx, err, ErrDatabaseOperation)
		}
		claim.OwnerToken, claim.Version = owner, version
		return app.BeginResult{Status: app.BeginClaimed, Claim: claim, RetryCurrent: true}, true, nil
	case "ready":
		return app.BeginResult{Status: app.BeginReady, Claim: claim, RetryCurrent: true}, true, nil
	case "sending":
		if update.DeliveryLeaseUntil.Valid && update.DeliveryLeaseUntil.Time.After(conversation.Now) {
			return app.BeginResult{Status: app.BeginBusy, Claim: claim}, true, nil
		}
		if err := markUnknownTx(ctx, tx, update); err != nil {
			return app.BeginResult{}, false, err
		}
		key := domain.ConversationKey{ChatID: update.ChatID, ThreadID: update.ThreadID}
		releaseErr := releaseConversation(ctx, tx, key, update.ProcessingOwner, update.ProcessingVersion)
		if releaseErr != nil && !errors.Is(releaseErr, ErrLeaseLost) {
			return app.BeginResult{}, false, releaseErr
		}
		refreshed, err := lockConversation(ctx, tx, key)
		if err != nil {
			return app.BeginResult{}, false, err
		}
		*conversation = refreshed
		if errors.Is(releaseErr, ErrLeaseLost) && leaseActive(*conversation) {
			return app.BeginResult{Status: app.BeginBusy}, true, nil
		}
		return app.BeginResult{}, false, nil
	default:
		return app.BeginResult{}, false, ErrDatabaseOperation
	}
}

func insertArchivedUserMessage(ctx context.Context, tx pgx.Tx, key domain.ConversationKey, updateID int64, text string) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO conversation_messages (chat_id, thread_id, update_id, archive_order, role, content)
		VALUES ($1, $2, $3, (SELECT COALESCE(MAX(archive_order), 0) + 1 FROM conversation_messages WHERE chat_id = $1 AND thread_id = $2), 'user', $4)
		ON CONFLICT (chat_id, thread_id, update_id, role) DO NOTHING`,
		key.ChatID, key.ThreadID, updateID, text); err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	return nil
}

func (s *Store) cleanupStale(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM processed_updates
		WHERE update_id IN (
			SELECT update_id FROM processed_updates
			WHERE state IN ('sent', 'delivery_unknown')
			  AND updated_at < clock_timestamp() - ($1::bigint * interval '1 microsecond')
			ORDER BY updated_at
			LIMIT $2
		)`, processedUpdateRetention.Microseconds(), cleanupBatchSize)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	return nil
}

func lockConversation(ctx context.Context, tx pgx.Tx, key domain.ConversationKey) (conversationRecord, error) {
	var record conversationRecord
	err := tx.QueryRow(ctx, `
		SELECT version, lease_owner, lease_until, clock_timestamp()
		FROM conversations
		WHERE chat_id = $1 AND thread_id = $2
		FOR UPDATE`, key.ChatID, key.ThreadID).Scan(
		&record.Version, &record.LeaseOwner, &record.LeaseUntil, &record.Now,
	)
	if err != nil {
		return conversationRecord{}, databaseError(ctx, err, ErrDatabaseOperation)
	}
	return record, nil
}

func lockUpdate(ctx context.Context, tx pgx.Tx, updateID int64) (updateRecord, error) {
	var record updateRecord
	err := tx.QueryRow(ctx, `
		SELECT update_id, chat_id, thread_id, source_message_id, input_text, kind, state,
		       processing_owner, processing_version, response_text, telegram_message_id,
		       delivery_owner, delivery_lease_until
		FROM processed_updates
		WHERE update_id = $1
		FOR UPDATE`, updateID).Scan(
		&record.UpdateID, &record.ChatID, &record.ThreadID, &record.SourceMessageID,
		&record.InputText, &record.Kind, &record.State, &record.ProcessingOwner,
		&record.ProcessingVersion, &record.ResponseText, &record.TelegramMessageID,
		&record.DeliveryOwner, &record.DeliveryLeaseUntil,
	)
	return record, err
}

func lockPending(ctx context.Context, tx pgx.Tx, key domain.ConversationKey, currentUpdateID int64) (updateRecord, error) {
	var record updateRecord
	err := tx.QueryRow(ctx, `
		SELECT update_id, chat_id, thread_id, source_message_id, input_text, kind, state,
		       processing_owner, processing_version, response_text, telegram_message_id,
		       delivery_owner, delivery_lease_until
		FROM processed_updates
		WHERE chat_id = $1 AND thread_id = $2 AND update_id <> $3
		  AND state IN ('processing', 'ready', 'sending')
		ORDER BY update_id
		LIMIT 1
		FOR UPDATE`, key.ChatID, key.ThreadID, currentUpdateID).Scan(
		&record.UpdateID, &record.ChatID, &record.ThreadID, &record.SourceMessageID,
		&record.InputText, &record.Kind, &record.State, &record.ProcessingOwner,
		&record.ProcessingVersion, &record.ResponseText, &record.TelegramMessageID,
		&record.DeliveryOwner, &record.DeliveryLeaseUntil,
	)
	return record, err
}

func setConversationLease(ctx context.Context, tx pgx.Tx, key domain.ConversationKey, version int64, owner string, duration time.Duration) error {
	_, err := tx.Exec(ctx, `
		UPDATE conversations
		SET version = $3, lease_owner = $4,
		    lease_until = clock_timestamp() + ($5::bigint * interval '1 microsecond'),
		    updated_at = clock_timestamp()
		WHERE chat_id = $1 AND thread_id = $2`, key.ChatID, key.ThreadID, version, owner, duration.Microseconds())
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	return nil
}

func releaseConversation(ctx context.Context, tx pgx.Tx, key domain.ConversationKey, owner string, version int64) error {
	result, err := tx.Exec(ctx, `
		UPDATE conversations
		SET version = version + 1, lease_owner = NULL, lease_until = NULL, updated_at = clock_timestamp()
		WHERE chat_id = $1 AND thread_id = $2 AND lease_owner = $3 AND version = $4`, key.ChatID, key.ThreadID, owner, version)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	if result.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

func markUnknownTx(ctx context.Context, tx pgx.Tx, update updateRecord) error {
	result, err := tx.Exec(ctx, `
		UPDATE processed_updates
		SET state = 'delivery_unknown', delivery_owner = NULL,
		    delivery_lease_until = NULL, updated_at = clock_timestamp()
		WHERE update_id = $1 AND state = 'sending'`, update.UpdateID)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	if _, err := tx.Exec(ctx, `
		UPDATE conversations
		SET last_update_id = GREATEST(last_update_id, $3), updated_at = clock_timestamp()
		WHERE chat_id = $1 AND thread_id = $2`, update.ChatID, update.ThreadID, update.UpdateID); err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	return nil
}

func validIncoming(incoming domain.Incoming) bool {
	return incoming.UpdateID >= 0 && incoming.Key.ChatID != 0 && incoming.Key.ThreadID >= 0 && incoming.MessageID > 0 && strings.TrimSpace(incoming.Text) != "" && len(incoming.Text) <= app.MaxConversationTextBytes && utf8.ValidString(incoming.Text) && !strings.ContainsRune(incoming.Text, '\x00') && (incoming.Kind == domain.MessageText || incoming.Kind == domain.MessageAbout)
}

func matchesIncoming(record updateRecord, incoming domain.Incoming) bool {
	return record.ChatID == incoming.Key.ChatID && record.ThreadID == incoming.Key.ThreadID && record.SourceMessageID == incoming.MessageID && record.InputText == incoming.Text && record.Kind == kindName(incoming.Kind)
}

func kindName(kind domain.MessageKind) string {
	if kind == domain.MessageAbout {
		return "about"
	}
	return "text"
}

func incomingFromRecord(record updateRecord) domain.Incoming {
	kind := domain.MessageText
	if record.Kind == "about" {
		kind = domain.MessageAbout
	}
	return domain.Incoming{
		UpdateID:  record.UpdateID,
		Key:       domain.ConversationKey{ChatID: record.ChatID, ThreadID: record.ThreadID},
		MessageID: record.SourceMessageID,
		Text:      record.InputText,
		Kind:      kind,
	}
}

func claimFromIncoming(incoming domain.Incoming, owner string, version int64) app.Claim {
	return app.Claim{Incoming: incoming, OwnerToken: owner, Version: version}
}

func claimFromRecord(record updateRecord) app.Claim {
	return claimFromIncoming(incomingFromRecord(record), record.ProcessingOwner, record.ProcessingVersion)
}

func leaseActive(conversation conversationRecord) bool {
	return conversation.LeaseUntil.Valid && conversation.LeaseUntil.Time.After(conversation.Now)
}

func ownerToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

func databaseError(ctx context.Context, err error, fallback error) error {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return fallback
}

func safeQueryError(ctx context.Context, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUpdateNotFound
	}
	return databaseError(ctx, err, ErrDatabaseOperation)
}

func validateDuration(duration time.Duration) bool {
	return duration > 0 && duration <= time.Hour
}
